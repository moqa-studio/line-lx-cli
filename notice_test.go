package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// noticeOf runs the notice as the hub's session hook does, and returns what it printed. It must always return nil.
func noticeOf(t *testing.T, slug string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runNotice(claudeTarget, slug, false, &out); err != nil {
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
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") || !strings.HasPrefix(out, `{"systemMessage":`) {
		t.Fatalf("not one systemMessage line: %q", out)
	}
	var doc map[string]string
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc) != 1 || doc["systemMessage"] == "" {
		t.Fatalf("not a lone systemMessage: %q (%v)", out, err)
	}
	return doc["systemMessage"]
}

// noticeAnswer is the fixture hub's answer with when each latest version came out, and notes' changelog.
func noticeAnswer(hub, changelog string) string {
	quoted, _ := json.Marshal(changelog)
	a := statusAnswer(hub)
	a = strings.ReplaceAll(a, `"latestVersion": "1.2.0",`, `"latestVersion": "1.2.0", "latestPublishedAt": "2026-11-01T10:00:00Z", "changelog": `+string(quoted)+`,`)
	a = strings.ReplaceAll(a, `"latestVersion": "1.1.0",`, `"latestVersion": "1.1.0", "latestPublishedAt": "2026-11-10T10:00:00Z", "changelog": "Lint faster.",`)
	return strings.ReplaceAll(a, `"latestVersion": "0.1.0",`, `"latestVersion": "0.1.0", "latestPublishedAt": "2026-11-12T10:00:00Z", "changelog": "",`)
}

// turnOff turns a plugin of the fixture's marketplace off in Claude Code's user settings, as the person can.
func turnOff(t *testing.T, plugin string) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
	putFile(t, path, strings.Replace(read(t, path), `"tools@metro-dev": true`, `"tools@metro-dev": true, "`+plugin+`@metro-dev": false`, 1))
}

// noRefresh replaces the refresh the notice starts on its own, and counts the starts.
func noRefresh(t *testing.T) *int {
	t.Helper()
	starts := 0
	was := startRefresh
	startRefresh = func(string) error { starts++; return nil }
	t.Cleanup(func() { startRefresh = was })
	return &starts
}

// setTold sets what the notice told last for the fixture's hub and workspace.
func setTold(t *testing.T, hub string, told map[string]string) {
	t.Helper()
	if err := updateNoticeState(time.Second, func(s *noticeState) bool {
		s.touchSpace(hub, "dev", time.Now()).Told = told
		return true
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNoticeSaysNothingWhenAllIsWell(t *testing.T) {
	noRefresh(t)
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	// The hub's latest is what Claude Code has (tools 1.0.0), and the person turned extra off.
	h.answer = strings.Replace(noticeAnswer(h.URL, "Sum up."), `"latestVersion": "1.1.0"`, `"latestVersion": "1.0.0"`, 1)
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
		if err := runNotice(tc[0], tc[1], false, &out); err != nil || out.Len() > 0 {
			t.Errorf("notice %v = %q, %v", tc, out.String(), err)
		}
	}
	if len(h.requests) != before {
		t.Errorf("asked the hub: %v", h.requests[before:])
	}
}

func TestNoticeTellsWhatIsWrongWithTheFix(t *testing.T) {
	noRefresh(t)
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	h.answer = noticeAnswer(h.URL, "Sum up.")
	// tools is behind (1.0.0 for 1.1.0, out since 10 Nov) and extra isn't installed (out since 12 Nov).
	got := messageOf(t, noticeOf(t, "dev"))
	if want := "Metro-LX: extra isn't installed yet. Run `line-lx sync`. (1 more problem: run `line-lx doctor`.)"; got != want {
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
	answer := &hubFacts{actsIn: "dev", plugins: map[string]latestRecord{
		"a": {Version: "2.0.0", Published: &old, Order: 0},
		"b": {Version: "1.0.0", Published: &old, Order: 1},
		"c": {Version: "3.0.0", Published: &fresh, Order: 2}, // out for an hour: on its way
	}}
	local := claudeLocal{plugins: map[string]localPlugin{"a": {version: "1.0.0"}, "c": {version: "2.0.0"}}}
	f := noticeFacts{hub: "https://hub.example.com", slug: "dev", answer: answer, local: local, now: now, claude: "2.1.300 (Claude Code)",
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
	// Another workspace: the sign-in acts elsewhere, and that workspace's plugins say nothing about these.
	f.signIn, f.claude = nil, "2.1.320"
	f.answer = &hubFacts{actsIn: "other", plugins: answer.plugins}
	if got := noticeProblems(f); len(got) != 1 || got[0].text != "Claude Code has the plugins of dev, but your line-lx sign-in now acts in other." {
		t.Errorf("workspace: %v", got)
	}
	// Without the hub's answer, nothing is said about the plugins.
	f.answer = nil
	if got := noticeProblems(f); len(got) != 0 {
		t.Errorf("offline: %v", got)
	}
}

func TestNoticeTellsUpdatesOnce(t *testing.T) {
	noRefresh(t)
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	hub, _ := checkHub(h.URL)
	// A changelog with escape sequences, a hyperlink and bidi overrides, and quotes: one clean line, quoted.
	h.answer = noticeAnswer(h.URL, "## What's new\n\n- \x1b[31mThe \"summary\" skill\x1b]8;;https://evil.example\x07 reads \xe2\x80\xaebackwards\xe2\x80\xac too.\nMore.")
	turnOff(t, "extra")

	// The first session only remembers: nothing is new yet.
	if out := noticeOf(t, "dev"); strings.Contains(out, "updated") {
		t.Errorf("first session: %q", out)
	}
	// notes was 1.1.0 when last told.
	setTold(t, hub, map[string]string{"notes": "1.1.0", "tools": "1.0.0"})
	got := messageOf(t, noticeOf(t, "dev"))
	want := "Metro-LX: tools is behind the hub for over a day. Send a message: Claude Code updates within 10 minutes. Then run `/reload-plugins`.\n" +
		`Metro-LX · notes updated to v1.2.0 · its changelog: "The 'summary' skill reads backwards too."`
	if got != want {
		t.Errorf("notice:\n got %q\nwant %q", got, want)
	}
	// Once.
	if got := messageOf(t, noticeOf(t, "dev")); strings.Contains(got, "notes") {
		t.Errorf("told twice: %q", got)
	}
	// A downgrade isn't an update: remembered, not told.
	setTold(t, hub, map[string]string{"notes": "1.3.0", "tools": "1.0.0"})
	if got := messageOf(t, noticeOf(t, "dev")); strings.Contains(got, "notes") {
		t.Errorf("downgrade told: %q", got)
	}
	if s := readNoticeState(); s.space(hub, "dev").Told["notes"] != "1.2.0" {
		t.Errorf("after a downgrade: %v", s.space(hub, "dev").Told)
	}

	// Another workspace on the same machine has its own record: neither resets the other.
	if err := updateNoticeState(time.Second, func(s *noticeState) bool {
		s.touchSpace(hub, "other", time.Now()).Told = map[string]string{"x": "1.0.0"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	setTold(t, hub, map[string]string{"notes": "1.1.0", "tools": "1.0.0"})
	_ = noticeOf(t, "dev")
	s := readNoticeState()
	if s.space(hub, "other") == nil || s.space(hub, "dev").Told["notes"] != "1.2.0" {
		t.Errorf("spaces = %+v", s.Spaces)
	}
	path, _ := noticePath("notice.json")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file: %v, %v", info, err)
	}

	// Without the hub's answer, an update waits for one that knows its version, to be told with its changelog.
	setTold(t, hub, map[string]string{"notes": "1.1.0", "tools": "1.0.0"})
	h.mu.Lock()
	h.status = http.StatusServiceUnavailable
	h.mu.Unlock()
	if err := updateNoticeState(time.Second, func(s *noticeState) bool { s.space(hub, "dev").Latest = nil; return true }); err != nil {
		t.Fatal(err)
	}
	if got := messageOf(t, noticeOf(t, "dev")); strings.Contains(got, "notes") {
		t.Errorf("without the hub: %q", got)
	}
	h.mu.Lock()
	h.status = http.StatusOK
	h.mu.Unlock()
	if got := messageOf(t, noticeOf(t, "dev")); !strings.Contains(got, `notes updated to v1.2.0 · its changelog: "The`) {
		t.Errorf("after the hub came back: %q", got)
	}
}

func TestNoticeKeepsToItsBudget(t *testing.T) {
	noRefresh(t)
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

// oauthHub is a hub with a token endpoint that answers after delay, with status, and GET /api/me/installed.
type oauthHub struct {
	*httptest.Server
	mu      sync.Mutex
	delay   time.Duration
	status  int
	refresh int
}

func newOAuthHub(t *testing.T) *oauthHub {
	h := &oauthHub{status: http.StatusOK}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": h.URL, "authorization_endpoint": h.URL + "/authorize",
				"token_endpoint": h.URL + "/token", "registration_endpoint": h.URL + "/register"})
		case "/token":
			h.mu.Lock()
			h.refresh++
			delay, status := h.delay, h.status
			h.mu.Unlock()
			time.Sleep(delay)
			w.WriteHeader(status)
			if status == http.StatusOK {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		case "/api/me/installed":
			if r.Header.Get("Authorization") != "Bearer fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(noticeAnswer(h.URL, "Sum up.")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

// The notice never refreshes the sign-in: a hub slow to answer the refresh would make it stop half-way, after the
// hub rotated the refresh token. It starts the refresh in a process of its own, which keeps the rotated token
// however long the hub takes, and records the hub's answer for the next session.
func TestNoticeNeverHoldsARefresh(t *testing.T) {
	keyring.MockInit()
	h := newOAuthHub(t)
	statusMachine(t, h.URL)
	t.Setenv("LINE_LX_TOKEN", "")
	hub, _ := checkHub(h.URL)
	expired := &creds{ClientID: "c", AccessToken: "old", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Hour), Resource: hub + "/api"}
	if err := saveCreds(hub, expired); err != nil {
		t.Fatal(err)
	}
	h.delay = 2 * time.Second // the reviewer's 7 s, shortened: past the notice's whole budget
	starts := noRefresh(t)

	start := time.Now()
	out := noticeOf(t, "dev")
	if took := time.Since(start); took > time.Second {
		t.Errorf("the notice took %s", took)
	}
	h.mu.Lock()
	asked := h.refresh
	h.mu.Unlock()
	if asked != 0 || *starts != 1 {
		t.Errorf("the notice refreshed %d times itself, started %d refreshes", asked, *starts)
	}
	if out != "" {
		t.Errorf("notice: %q", out) // nothing recorded yet, nothing local wrong
	}

	// The refresh, as the started process runs it: it waits for the hub, and saves what it rotated.
	if err := runNotice(claudeTarget, "dev", true, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if c, _ := loadCreds(hub); c == nil || c.AccessToken != "fresh" || c.RefreshToken != "rotated" {
		t.Fatalf("after the refresh: %+v", c)
	}
	// The next session, signed in: it asks the hub itself, and tells what's behind.
	if got := messageOf(t, noticeOf(t, "dev")); !strings.Contains(got, "extra isn't installed yet") {
		t.Errorf("next session: %q", got)
	}
	// Expired again, with the hub answering slowly: the recorded answer still tells what's behind.
	if err := saveCreds(hub, expired); err != nil {
		t.Fatal(err)
	}
	if got := messageOf(t, noticeOf(t, "dev")); !strings.Contains(got, "extra isn't installed yet") {
		t.Errorf("from the recorded answer: %q", got)
	}

	// The hub refuses the refresh: the next session says so, until a new sign-in.
	h.mu.Lock()
	h.delay, h.status = 0, http.StatusBadRequest
	h.mu.Unlock()
	if err := runNotice(claudeTarget, "dev", true, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if got := messageOf(t, noticeOf(t, "dev")); !strings.HasPrefix(got, "Metro-LX: Claude Code gets no plugin updates: your line-lx sign-in expired. Run `line-lx login`.") {
		t.Errorf("after a refused refresh: %q", got)
	}
	renewed := *expired
	renewed.ExpiresAt = time.Now().Add(-time.Minute) // another sign-in since
	if err := saveCreds(hub, &renewed); err != nil {
		t.Fatal(err)
	}
	if got := messageOf(t, noticeOf(t, "dev")); strings.Contains(got, "expired") {
		t.Errorf("after a new sign-in: %q", got)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.0", 1}, {"1.2.0", "1.2.0", 0}, {"1.2.0-rc.1", "1.2.0", -1}, {"2.0.0", "10.0.0", -1}, {"abc", "1.0.0", 0},
	} {
		if got := compareVersions(c.a, c.b); got != c.want && !(got > 0 && c.want > 0) && !(got < 0 && c.want < 0) {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestChangelogLine(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "",
		"\n\n  \n":                            "",
		"## 1.2.0\n- Adds deploy":             "Adds deploy",
		"- Adds \x1b[1mdeploy\x1b[0m.\nMore.": "Adds deploy.",
		"\xe2\x80\xaeevil\xe2\x80\xac \xe2\x80\x8bzero": "evil zero",
		"> quoted\ttab":          "quoted tab",
		strings.Repeat("é", 200): strings.Repeat("é", maxChangelog-1) + "…",
		"\x1b]8;;https://evil.example\x07link\x1b]8;;\x07": "link",
	} {
		if got := changelogLine(&in); got != want {
			t.Errorf("changelogLine(%q) = %q, want %q", in, got, want)
		}
	}
}
