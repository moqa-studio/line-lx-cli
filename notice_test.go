package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noticeOf runs the notice as the hub's session hook does, and returns what it printed. It must always return nil.
func noticeOf(t *testing.T, slug string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runNotice(claudeTarget, slug, &out); err != nil {
		t.Fatalf("notice returned %v", err)
	}
	return out.String()
}

// messageOf reads the notice's output: one JSON object, with systemMessage and nothing else, on one line.
func messageOf(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("not one line: %q", out)
	}
	var doc map[string]string
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc) != 1 || doc["systemMessage"] == "" {
		t.Fatalf("not a lone systemMessage: %q (%v)", out, err)
	}
	return doc["systemMessage"]
}

// noticeAnswer is the fixture hub's answer with when each latest version came out, and notes' changelog.
func noticeAnswer(hub, changelog string) string {
	a := statusAnswer(hub)
	a = strings.ReplaceAll(a, `"latestVersion": "1.2.0",`, `"latestVersion": "1.2.0", "latestPublishedAt": "2026-11-01T10:00:00Z", "changelog": `+changelog+`,`)
	a = strings.ReplaceAll(a, `"latestVersion": "1.1.0",`, `"latestVersion": "1.1.0", "latestPublishedAt": "2026-11-10T10:00:00Z", "changelog": "Lint faster.",`)
	return strings.ReplaceAll(a, `"latestVersion": "0.1.0",`, `"latestVersion": "0.1.0", "latestPublishedAt": "2026-11-12T10:00:00Z", "changelog": "",`)
}

// turnOff turns a plugin of the fixture's marketplace off in Claude Code's user settings, as the person can.
func turnOff(t *testing.T, plugin string) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
	putFile(t, path, strings.Replace(read(t, path), `"tools@metro-dev": true`, `"tools@metro-dev": true, "`+plugin+`@metro-dev": false`, 1))
}

func TestNoticeSaysNothingWhenAllIsWell(t *testing.T) {
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	// The hub's latest is what Claude Code has (tools 1.0.0), and the person turned extra off.
	h.answer = strings.Replace(noticeAnswer(h.URL, `"Sum up."`), `"latestVersion": "1.1.0"`, `"latestVersion": "1.0.0"`, 1)
	turnOff(t, "extra")
	for range 2 {
		if out := noticeOf(t, "dev"); out != "" {
			t.Errorf("notice printed %q", out)
		}
	}
	// An agent it doesn't know, or a workspace that isn't a name: nothing, not even a look at the hub.
	before := len(h.requests)
	for _, tc := range [][2]string{{"codex", "dev"}, {claudeTarget, "../dev"}, {claudeTarget, ""}} {
		var out bytes.Buffer
		if err := runNotice(tc[0], tc[1], &out); err != nil || out.Len() > 0 {
			t.Errorf("notice %v = %q, %v", tc, out.String(), err)
		}
	}
	if len(h.requests) != before {
		t.Errorf("asked the hub: %v", h.requests[before:])
	}
}

func TestNoticeTellsWhatIsWrongWithTheFix(t *testing.T) {
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	h.answer = noticeAnswer(h.URL, `"Sum up."`)
	// tools is behind (1.0.0 for 1.1.0, out since 10 Nov) and extra isn't installed (out since 12 Nov).
	got := messageOf(t, noticeOf(t, "dev"))
	want := "Metro-LX: extra isn't installed yet. Run `line-lx sync`. (1 more problem: run `line-lx doctor`.)"
	if got != want {
		t.Errorf("notice:\n got %q\nwant %q", got, want)
	}
	// Turned off in Claude Code's settings, extra isn't the person's problem.
	turnOff(t, "extra")
	got = messageOf(t, noticeOf(t, "dev"))
	if want := "Metro-LX: tools is behind the hub for over a day. Send a message: Claude Code updates within 10 minutes. Then run `/reload-plugins`."; got != want {
		t.Errorf("notice:\n got %q\nwant %q", got, want)
	}

	// The hub refusing the sign-in comes first: nothing else reaches Claude Code until it's fixed.
	h.mu.Lock()
	h.status = http.StatusUnauthorized
	h.mu.Unlock()
	got = messageOf(t, noticeOf(t, "dev"))
	if !strings.HasPrefix(got, "Metro-LX: Claude Code gets no plugin updates: your line-lx sign-in was refused by the hub. Run `line-lx login`.") {
		t.Errorf("notice: %q", got)
	}
	if strings.Contains(got, statusToken) {
		t.Error("the token leaked")
	}
}

func TestNoticeProblemsInOrder(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	fresh := now.Add(-time.Hour)
	hv := hubView{status: hubOK, workspace: &statusWorkspace{Slug: "dev"}, order: map[string]int{"a": 0, "b": 1, "c": 2},
		agents: map[string]hubAgentView{claudeTarget: {plugins: map[string]hubPluginView{
			"a": {latest: ptr("2.0.0"), published: &old},
			"b": {latest: ptr("1.0.0"), published: &old},
			"c": {latest: ptr("3.0.0"), published: &fresh}, // out for an hour: on its way
		}}}}
	local := claudeLocal{plugins: map[string]localPlugin{"a": {version: "1.0.0"}, "c": {version: "2.0.0"}}}
	f := noticeFacts{hub: "https://hub.example.com", slug: "dev", hv: hv, local: local, now: now, claude: "2.1.300 (Claude Code)",
		signIn: errNotSignedIn{"", "from an earlier line-lx"}}
	var texts []string
	for _, n := range noticeProblems(f) {
		texts = append(texts, n.text+" "+n.fix)
	}
	want := []string{
		"Claude Code gets no plugin updates: your line-lx sign-in is from an earlier line-lx. Run `line-lx login`.",
		"Claude Code 2.1.300 is older than 2.1.319, so new plugins won't arrive. Run `claude update`.",
		"b isn't installed yet. Run `line-lx sync`.",
		"a is behind the hub for over a day. Send a message: Claude Code updates within 10 minutes. Then run `/reload-plugins`.",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems:\n%s\nwant\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	if msg := noticeMessage(noticeProblems(f), nil); !strings.HasSuffix(msg, "(3 more problems: run `line-lx doctor`.)") || strings.Contains(msg, "\n") {
		t.Errorf("message: %q", msg)
	}
	// Another workspace: the sign-in acts elsewhere.
	f.signIn, f.claude, f.hv.workspace = nil, "2.1.320", &statusWorkspace{Slug: "other"}
	if got := noticeProblems(f)[0].text; got != "Claude Code has the plugins of dev, but your line-lx sign-in now acts in other." {
		t.Errorf("workspace: %q", got)
	}
	// Without the hub's answer, nothing is said about the plugins.
	f.hv = hubView{status: hubUnreachable}
	if got := noticeProblems(f); len(got) != 0 {
		t.Errorf("offline: %v", got)
	}
}

func TestNoticeTellsUpdatesOnce(t *testing.T) {
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	hub, _ := checkHub(h.URL)
	h.answer = noticeAnswer(h.URL, `"## What's new\n\n- \u001b[31mThe summary skill\u001b]8;;https://evil.example\u0007 reads \u202ebackwards\u202c too.\nMore."`)
	turnOff(t, "extra")

	// The first session only remembers: nothing is new yet.
	if out := noticeOf(t, "dev"); strings.Contains(out, "updated") {
		t.Errorf("first session: %q", out)
	}
	// notes was 1.1.0 at the last session.
	if err := updateNoticeState(func(s *noticeState) bool { s.Plugins["notes"] = "1.1.0"; return true }); err != nil {
		t.Fatal(err)
	}
	got := messageOf(t, noticeOf(t, "dev"))
	want := "Metro-LX: tools is behind the hub for over a day. Send a message: Claude Code updates within 10 minutes. Then run `/reload-plugins`.\n" +
		"Metro-LX updated notes to v1.2.0: The summary skill reads backwards too."
	if got != want {
		t.Errorf("notice:\n got %q\nwant %q", got, want)
	}
	// Once.
	if got := messageOf(t, noticeOf(t, "dev")); strings.Contains(got, "notes") {
		t.Errorf("told twice: %q", got)
	}
	// What the state holds: versions, and nothing else.
	s := readNoticeState()
	if s.Hub != hub || s.Workspace != "dev" || len(s.Plugins) != 2 || s.Plugins["notes"] != "1.2.0" || s.Plugins["tools"] != "1.0.0" {
		t.Errorf("state = %+v", s)
	}
	path, _ := noticePath("notice.json")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file: %v, %v", info, err)
	}

	// Without the hub's answer, an update waits for the next session, to be told with its changelog.
	if err := updateNoticeState(func(s *noticeState) bool { s.Plugins["notes"] = "1.1.0"; return true }); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.status = http.StatusServiceUnavailable
	h.mu.Unlock()
	if out := noticeOf(t, "dev"); out != "" {
		t.Errorf("without the hub: %q", out)
	}
	h.mu.Lock()
	h.status = http.StatusOK
	h.mu.Unlock()
	if got := messageOf(t, noticeOf(t, "dev")); !strings.Contains(got, "Metro-LX updated notes to v1.2.0: The summary skill") {
		t.Errorf("after the hub came back: %q", got)
	}
}

func TestNoticeKeepsToItsBudget(t *testing.T) {
	release := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(hub.Close)
	t.Cleanup(func() { close(release) })
	statusMachine(t, hub.URL)
	wait := noticeWait
	noticeWait = 300 * time.Millisecond
	t.Cleanup(func() { noticeWait = wait })
	// A Claude Code too old, which answers at once: what is known locally is still told.
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	putFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\necho '2.1.300 (Claude Code)'\n")
	if err := os.Chmod(filepath.Join(bin, "claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	got := messageOf(t, noticeOf(t, "dev"))
	if took := time.Since(start); took > time.Second {
		t.Errorf("the notice took %s", took)
	}
	if want := "Metro-LX: Claude Code 2.1.300 is older than 2.1.319, so new plugins won't arrive. Run `claude update`."; got != want {
		t.Errorf("notice: %q", got)
	}
	// Claude Code's version is remembered for its binary: `claude --version` runs again only when it changes.
	if s := readNoticeState(); s.Claude == nil || s.Claude.Version != "2.1.300" {
		t.Errorf("state = %+v", s.Claude)
	}
}

func TestRefreshGate(t *testing.T) {
	var gate atomic.Int32
	gate.Store(gateRefresh)
	go func() {
		time.Sleep(100 * time.Millisecond)
		gate.Store(gateOpen)
	}()
	start := time.Now()
	if !closeGate(&gate, time.Now().Add(time.Second)) || time.Since(start) < 100*time.Millisecond {
		t.Error("closeGate didn't wait for the refresh under way")
	}
	// Closed, no refresh starts: the sign-in stays as it is.
	t.Setenv("LINE_LX_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	if _, err := watchedToken("https://hub.example.com", &gate); err != nil && !errors.Is(err, errGateClosed) {
		var none errNotSignedIn
		if !errors.As(err, &none) {
			t.Errorf("watchedToken = %v", err)
		}
	}
	gate.Store(gateRefresh)
	if closeGate(&gate, time.Now().Add(50*time.Millisecond)) {
		t.Error("closeGate said a refresh under way had ended")
	}
}

func TestChangelogLine(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "",
		"\n\n  \n":                            "",
		"## 1.2.0\n- Adds deploy":             "Adds deploy",
		"- Adds \x1b[1mdeploy\x1b[0m.\nMore.": "Adds deploy.",
		"\u202eevil\u202c \u200bzero":         "evil zero",
		"> quoted\ttab":                       "quoted tab",
		strings.Repeat("é", 200):              strings.Repeat("é", maxChangelog-1) + "…",
		"\x1b]8;;https://evil.example\x07link\x1b]8;;\x07": "link",
	} {
		if got := changelogLine(&in); got != want {
			t.Errorf("changelogLine(%q) = %q, want %q", in, got, want)
		}
	}
}
