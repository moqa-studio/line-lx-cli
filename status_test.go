package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// statusNow is the clock of the rendering tests: the fixture's times are relative to it.
var statusNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// statusToken is the person's token: distinct enough that finding it in an output means it leaked.
const statusToken = "s3cret-tok3n-x9"

// statusHub answers GET /api/me/installed with answer (the fixture's by default) and status, GET /api/me, and
// GET /api/usage/mine and /api/usage/settings with usage and settings, and records the requests it gets.
type statusHub struct {
	*httptest.Server
	mu              sync.Mutex
	status          int
	answer          string
	usage, settings string
	requests        []string
}

func newStatusHub(t *testing.T) *statusHub {
	h := &statusHub{status: http.StatusOK, usage: usageAnswer, settings: `{"agentReporting": true, "perPersonView": false}`}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requests = append(h.requests, r.Method+" "+r.URL.String()+" "+r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+statusToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/me":
			fmt.Fprint(w, `{"user": {"name": "Robin", "email": "robin@example.com"}, "org": {"name": "Dev", "slug": "dev"}, "role": "owner"}`)
		case "GET /api/usage/mine":
			fmt.Fprint(w, h.usage)
		case "GET /api/usage/settings":
			fmt.Fprint(w, h.settings)
		case "GET /api/me/installed":
			w.WriteHeader(h.status)
			fmt.Fprint(w, h.answer)
		default:
			http.NotFound(w, r)
		}
	}))
	h.answer = statusAnswer(h.URL)
	t.Cleanup(h.Close)
	return h
}

// statusAnswer is the hub's view of the fixture machine's person: notes 1.2.0 (route 1), tools 1.1.0 (route 2)
// and extra 0.1.0 (route 3), in the hub's order, for every agent, and the bundle for Claude Code, with the usage hook.
func statusAnswer(hub string) string {
	plugins := func(delivered string) string {
		return `[
      {"name": "notes", "route": 1, "line": "engineering", "latestVersion": "1.2.0", "deliveredVersion": "` + delivered + `", "firstDeliveredAt": "2026-11-01T10:00:00Z", "lastDeliveredAt": "2026-11-18T09:00:00Z", "behind": false,
       "skills": [{"name": "summary", "description": "Sum up the notes of the day", "review": {"status": "done", "label": "To review", "tone": "attention", "counts": {"critical": 0, "high": 0, "medium": 1, "low": 2}}},
         {"name": "jot", "description": "Jot a note", "review": {"status": "done", "label": "No findings", "tone": "ok", "counts": {"critical": 0, "high": 0, "medium": 0, "low": 0}}}], "hooks": []},
      {"name": "tools", "route": 2, "line": "product", "latestVersion": "1.1.0", "deliveredVersion": "1.0.0", "firstDeliveredAt": "2026-11-10T10:00:00Z", "lastDeliveredAt": "2026-11-10T10:00:00Z", "behind": true,
       "skills": [{"name": "lint", "description": "Lint the repository", "review": {"status": "done", "label": "Low notes · no secret scan", "tone": "neutral", "counts": {"critical": 1, "high": 0, "medium": 0, "low": 3}}}], "hooks": []},
      {"name": "extra", "route": 3, "line": "marketing", "latestVersion": "0.1.0", "deliveredVersion": null, "firstDeliveredAt": null, "lastDeliveredAt": null, "behind": false,
       "skills": [{"name": "help", "description": "Help", "review": {"status": "done", "label": "Flagged · Cisco in part", "tone": "danger", "counts": {"critical": 0, "high": 1, "medium": 0, "low": 0}}}], "hooks": []}`
	}
	return `{"workspace": {"slug": "dev", "name": "Dev"}, "hub": "` + hub + `", "agents": [
    {"target": "claude-code", "lastFetchedAt": "2026-11-19T08:00:00Z", "plugins": ` + plugins("1.2.0") + `,
      {"name": "metro-lx", "route": null, "line": null, "latestVersion": "a1b2c3d4e5f6", "deliveredVersion": "13cd9935af1d", "firstDeliveredAt": "2026-11-01T10:00:00Z", "lastDeliveredAt": "2026-11-12T10:00:00Z", "behind": true,
       "skills": [], "hooks": [{"event": "PostToolUse", "command": "line-lx report-usage --agent claude-code --workspace dev"}]}]},
    {"target": "codex", "lastFetchedAt": null, "plugins": ` + plugins("1.1.0") + `]},
    {"target": "pi", "lastFetchedAt": null, "plugins": ` + plugins("1.2.0") + `]},
    {"target": "claude", "lastFetchedAt": null, "plugins": ` + plugins("1.2.0") + `]}
  ]}`
}

// usageAnswer is the person's usage over the week: 53 runs reported by agents, 35 of notes (27 + 4 in Claude Code, 4
// in Codex) and 18 of tools, and what the web app doesn't count as runs: a delivery the hub saw, an MCP call, a failure.
const usageAnswer = `{"since": "2026-11-13", "rows": [
  {"pluginId": "p1", "plugin": "notes", "versionId": "v12", "version": "1.2.0", "review": "no_findings", "reviewJudged": true, "reviewIncomplete": [], "agent": "claude-code", "source": "agent", "kind": "activated", "uses": 27, "lastDay": "2026-11-19"},
  {"pluginId": "p1", "plugin": "notes", "versionId": "v11", "version": "1.1.0", "review": null, "reviewJudged": null, "reviewIncomplete": null, "agent": "claude-code", "source": "agent", "kind": "activated", "uses": 4, "lastDay": "2026-11-14"},
  {"pluginId": "p1", "plugin": "notes", "versionId": "v12", "version": "1.2.0", "review": "no_findings", "reviewJudged": true, "reviewIncomplete": [], "agent": "codex", "source": "agent", "kind": "activated", "uses": 4, "lastDay": "2026-11-18"},
  {"pluginId": "p2", "plugin": "tools", "versionId": "v10", "version": "1.0.0", "review": "review", "reviewJudged": true, "reviewIncomplete": [], "agent": "claude-code", "source": "agent", "kind": "activated", "uses": 18, "lastDay": "2026-11-19"},
  {"pluginId": "p2", "plugin": "tools", "versionId": "v10", "version": "1.0.0", "review": "review", "reviewJudged": true, "reviewIncomplete": [], "agent": "claude-code", "source": "agent", "kind": "failed", "uses": 2, "lastDay": "2026-11-19"},
  {"pluginId": "p1", "plugin": "notes", "versionId": "v12", "version": "1.2.0", "review": "no_findings", "reviewJudged": true, "reviewIncomplete": [], "agent": "claude-code", "source": "hub", "kind": "delivered", "uses": 3, "lastDay": "2026-11-18"},
  {"pluginId": "p3", "plugin": "extra", "versionId": "v01", "version": "0.1.0", "review": null, "reviewJudged": null, "reviewIncomplete": null, "agent": "mcp", "source": "hub", "kind": "mcp_call", "uses": 7, "lastDay": "2026-11-17"}
]}`

// statusMachine is a machine where Claude Code is connected to the hub's marketplace metro-dev and installed
// notes 1.2.0 (skills jot and summary), tools 1.0.0 (skill lint) and the bundle (the usage hook), and other
// from another marketplace; line-lx wrote notes 1.1.0 and old for Codex, and notes 1.2.0 for pi, with a
// plugin of another hub. It returns the home folder.
func statusMachine(t *testing.T, hub string) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir()) // no claude: Claude Code is found by its folder
	t.Setenv("LINE_LX_HUB", hub)
	t.Setenv("LINE_LX_TOKEN", statusToken)
	hub, _ = checkHub(hub)

	claude := filepath.Join(home, ".claude")
	cache := filepath.Join(claude, "plugins", "cache")
	notes := filepath.Join(cache, "metro-dev", "notes", "1.2.0")
	tools := filepath.Join(cache, "metro-dev", "tools", "1.0.0")
	bundle := filepath.Join(cache, "metro-dev", "metro-lx", "13cd9935af1d")
	other := filepath.Join(cache, "elsewhere", "other", "0.1.0")
	helper, err := helperCommand()
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(claude, "settings.json"), `{
  "extraKnownMarketplaces": {"metro-dev": {"source": {"source": "url", "url": "`+marketplaceURL(hub)+`", "headersHelper": "`+helper+`"}, "autoUpdate": true}},
  "enabledPlugins": {"metro-lx@metro-dev": true, "notes@metro-dev": true, "tools@metro-dev": true}
}`)
	putFile(t, filepath.Join(claude, "plugins", "known_marketplaces.json"), `{
  "metro-dev": {"source": {"source": "url", "url": "`+marketplaceURL(hub)+`", "headersHelper": "`+helper+`"}, "lastUpdated": "2026-11-19T10:00:00.000Z", "autoUpdate": true},
  "elsewhere": {"source": {"source": "github", "repo": "someone/plugins"}, "lastUpdated": "2026-11-19T10:00:00.000Z"}
}`)
	putFile(t, filepath.Join(claude, "plugins", "installed_plugins.json"), `{"version": 2, "plugins": {
  "notes@metro-dev": [{"scope": "user", "installPath": "`+notes+`", "version": "1.2.0", "installedAt": "2026-11-01T10:00:00.000Z", "lastUpdated": "2026-11-18T09:00:00.000Z"}],
  "tools@metro-dev": [
    {"scope": "project", "installPath": "`+filepath.Join(cache, "metro-dev", "tools", "0.9.0")+`", "version": "0.9.0", "installedAt": "2026-10-01T10:00:00.000Z", "lastUpdated": "2026-10-01T10:00:00.000Z"},
    {"scope": "user", "installPath": "`+tools+`", "version": "1.0.0", "installedAt": "2026-11-10T10:00:00.000Z", "lastUpdated": "2026-11-10T10:00:00.000Z"}],
  "metro-lx@metro-dev": [{"scope": "user", "installPath": "`+bundle+`", "version": "13cd9935af1d", "installedAt": "2026-11-01T10:00:00.000Z", "lastUpdated": "2026-11-12T10:00:00.000Z"}],
  "other@elsewhere": [{"scope": "user", "installPath": "`+other+`", "version": "0.1.0"}]
}}`)
	putFile(t, filepath.Join(notes, ".claude-plugin", "plugin.json"), `{"name": "notes", "version": "1.2.0"}`)
	putFile(t, filepath.Join(notes, "skills", "jot", "SKILL.md"), "---\nname: jot\ndescription: Jot a note, quickly\n---\nJot.\n")
	putFile(t, filepath.Join(notes, "skills", "summary", "SKILL.md"), "---\nname: summary\ndescription: >\n  Sum up.\n---\nSum up.\n")
	putFile(t, filepath.Join(tools, ".claude-plugin", "plugin.json"), `{"name": "tools", "version": "1.0.0"}`)
	putFile(t, filepath.Join(tools, "skills", "lint", "SKILL.md"), "---\nname: lint\n---\nLint.\n")
	putFile(t, filepath.Join(bundle, ".claude-plugin", "plugin.json"), `{"name": "metro-lx", "dependencies": ["notes", "tools"]}`)
	putFile(t, filepath.Join(bundle, "hooks", "hooks.json"), `{"description": "Metro-LX usage reporting", "hooks": {
  "PostToolUse": [{"matcher": "^Skill$", "hooks": [{"type": "command", "command": "line-lx report-usage --agent claude-code --workspace dev", "async": true}]}],
  "PostToolUseFailure": [{"matcher": "^Skill$", "hooks": [{"type": "command", "command": "line-lx report-usage --agent claude-code --workspace dev", "async": true}]}]
}}`)
	putFile(t, filepath.Join(other, "skills", "lint", "SKILL.md"), "---\nname: lint\n---\n")

	codex := filepath.Join(home, ".codex", "skills")
	putFile(t, filepath.Join(codex, ".line-lx", "notes.json"), `{"hub": "`+hub+`", "plugin": "notes", "version": "1.1.0", "versionId": "v11", "syncedAt": "2026-11-17T12:00:00Z", "skills": ["jot"]}`)
	putFile(t, filepath.Join(codex, "notes--jot", "SKILL.md"), "---\nname: jot\ndescription: Jot a note\n---\n")
	putFile(t, filepath.Join(codex, ".line-lx", "old-notes.json"), `{"hub": "`+hub+`", "plugin": "old-notes", "version": "2.0.0", "versionId": "v2", "syncedAt": "2026-09-01T12:00:00Z", "skills": ["gone"]}`)
	putFile(t, filepath.Join(codex, "old-notes--gone", "SKILL.md"), "---\nname: gone\n---\n")

	pi := filepath.Join(home, ".pi", "agent", "skills")
	putFile(t, filepath.Join(pi, "notes", ".line-lx.json"), `{"hub": "`+hub+`", "plugin": "notes", "version": "1.2.0", "versionId": "v12", "syncedAt": "2026-11-19T11:58:00Z"}`)
	putFile(t, filepath.Join(pi, "notes", "jot", "SKILL.md"), "---\nname: jot\ndescription: Jot a note\n---\n")
	putFile(t, filepath.Join(pi, "notes", "summary", "SKILL.md"), "---\nname: summary\n---\n")
	putFile(t, filepath.Join(pi, "theirs", ".line-lx.json"), `{"hub": "https://other.example.com", "plugin": "theirs", "version": "3.0.0", "versionId": "x", "syncedAt": "2026-11-19T11:00:00Z"}`)
	putFile(t, filepath.Join(pi, "theirs", "help-with-everything-they-offer", "SKILL.md"), "---\nname: help-with-everything-they-offer\ndescription: Helps with what the other hub offers, from start to end\n---\n")
	return home
}

func ptr[T any](v T) *T { return &v }

// TestHubTextIsCleaned: what the hub sends reaches the terminal without escape sequences, control or format
// characters, and a plugin, skill or agent whose name isn't a valid one is dropped.
func TestHubTextIsCleaned(t *testing.T) {
	var doc hubInstalled
	if err := json.Unmarshal([]byte(`{"workspace": {"slug": "dev", "name": "Dev\u001b[2J\u001b[1;31m Co"}, "agents": [
  {"target": "codex", "plugins": [
    {"name": "notes", "route": 2, "line": "engineering\u001b[31m", "latestVersion": "1.2.0", "deliveredVersion": "1.0.0; rm -rf",
     "skills": [{"name": "jot", "description": "Jot‮ a\n note\u001b]8;;http://x\u0007",
       "review": {"status": "done", "label": "No\u001b[31m fin\u0090dings‮\u001b]8;;http://x\u0007", "tone": "ok", "counts": {"medium": -3}}},
      {"name": "../evil", "description": "x"},
      {"name": "odd", "review": {"status": "done", "label": "Safe", "tone": "great"}},
      {"name": "odder", "review": {"status": "done", "label": "x", "tone": "ok", "counts": {"high": "many"}}}],
     "hooks": [{"event": "PostToolUse", "command": "line-lx\u0007 report-usage"}, {"event": "Bad Event", "command": "x"}]},
    {"name": "Not-Valid\u001b[31m", "latestVersion": "1.0.0"}]},
  {"target": "../x", "plugins": [{"name": "sneaky", "latestVersion": "1.0.0"}]}
]}`), &doc); err != nil {
		t.Fatal(err)
	}
	v := readHubView(doc)
	if v.workspace.Name != "Dev Co" {
		t.Errorf("workspace name = %q", v.workspace.Name)
	}
	notes := v.agents["codex"].plugins["notes"]
	// A review in a shape line-lx doesn't know (an unknown tone, a count that isn't one) is no review, never a failure.
	want := []statusSkill{{"jot", "Jot a note", &statusReview{Status: "done", Label: "No findings", Tone: "ok"}}, {"odd", "", nil}, {"odder", "", nil}}
	if !reflect.DeepEqual(notes.skills, want) {
		t.Errorf("skills = %+v, want %+v", notes.skills, want)
	}
	if want := []statusHook{{"PostToolUse", "line-lx report-usage"}}; !reflect.DeepEqual(notes.hooks, want) {
		t.Errorf("hooks = %+v, want %+v", notes.hooks, want)
	}
	if notes.delivered != nil {
		t.Errorf("a version that isn't one was kept: %q", *notes.delivered)
	}
	if len(v.agents) != 1 || len(v.latest) != 1 {
		t.Errorf("an invalid agent or plugin was kept: %v", v.latest)
	}
	if len(v.lines) != 0 {
		t.Errorf("a line line-lx doesn't know was kept: %v", v.lines)
	}
}

func TestMergePlugins(t *testing.T) {
	const hub = "https://hub.example.com"
	hv := hubView{status: hubOK, agents: map[string]hubAgentView{"codex": {plugins: map[string]hubPluginView{
		"current":     {latest: ptr("1.0.0")},
		"behind":      {latest: ptr("2.0.0")},
		"missing":     {latest: ptr("1.0.0")},
		"unpublished": {},
	}}}, latest: map[string]*string{"current": ptr("1.0.0"), "behind": ptr("2.0.0"), "missing": ptr("1.0.0"), "unpublished": nil}}
	local := map[string]localPlugin{
		"current": {version: "1.0.0", hub: hub},
		"behind":  {version: "1.0.0", hub: hub},
		"gone":    {version: "1.0.0", hub: hub},
		"foreign": {version: "1.0.0", hub: "https://other.example.com"},
	}
	want := map[string]string{"current": pluginUpToDate, "behind": pluginBehind, "missing": pluginMissing,
		"gone": pluginNotInHub, "foreign": pluginOtherHub} // unpublished and not here: nothing to show
	got := map[string]string{}
	for _, p := range mergePlugins("codex", hub, local, hv) {
		got[p.Name] = p.State
		if p.Behind != (p.State == pluginBehind) {
			t.Errorf("%s: behind %v, state %s", p.Name, p.Behind, p.State)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}

	// Without the hub's view, nothing is missing, behind or gone: only installed.
	for _, p := range mergePlugins("codex", hub, local, hubView{status: hubUnreachable}) {
		if p.State != pluginInstalled && p.State != pluginOtherHub {
			t.Errorf("without the hub, %s is %s", p.Name, p.State)
		}
	}

	// The bundle isn't a plugin of the hub: one the hub doesn't list is installed, not gone.
	plugins := mergePlugins(claudeTarget, hub, map[string]localPlugin{bundlePlugin: {version: "abc123", hub: hub}}, hubView{status: hubOK})
	if len(plugins) != 1 || plugins[0].State != pluginInstalled {
		t.Errorf("bundle = %+v", plugins)
	}
}

// TestStatusWithoutHub: signed out, a sign-in the hub refuses, a hub too old for the endpoint (404), one
// that fails or doesn't answer. Each time status shows what this machine has and why, and never the token.
func TestStatusWithoutHub(t *testing.T) {
	keyring.MockInit()
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	run := func(asJSON bool) string {
		return captureStdout(t, func() {
			if err := runStatus(asJSON); err != nil {
				t.Error(err)
			}
		})
	}
	check := func(name, wantStatus, wantNote string) {
		t.Helper()
		before := len(h.requests)
		out, js := run(false), run(true)
		var doc struct {
			HubStatus string
			Agents    []struct {
				Plugins []struct{ Name, State string }
			}
		}
		if err := json.Unmarshal([]byte(js), &doc); err != nil {
			t.Fatalf("%s: not JSON: %v", name, err)
		}
		if doc.HubStatus != wantStatus || !strings.Contains(out, wantNote) {
			t.Errorf("%s: hubStatus %q, want %q; output:\n%s", name, doc.HubStatus, wantStatus, out)
		}
		if strings.Contains(out+js, statusToken) {
			t.Errorf("%s: the token is in the output", name)
		}
		if wantStatus == hubSignedOut && len(h.requests) != before {
			t.Errorf("%s: a request went to the hub", name)
		}
		if wantStatus != hubOK && (len(doc.Agents[0].Plugins) != 3 || doc.Agents[0].Plugins[0].State != pluginInstalled) {
			t.Errorf("%s: Claude Code's plugins here = %+v", name, doc.Agents[0].Plugins)
		}
	}

	check("answer", hubOK, "▌▌▌ metro-lx  Dev")
	h.status, h.answer = http.StatusNotFound, `{"error": {"code": "not_found", "message": "Not found"}}`
	check("404", hubUnsupported, "The hub doesn't say what it delivered yet (it needs an update)")
	h.status, h.answer = http.StatusInternalServerError, `{"error": {"code": "internal_error", "message": "boom"}}`
	check("500", hubFailed, "The hub answered 500: boom")
	h.status, h.answer = http.StatusOK, `{"workspace":`
	check("bad JSON", hubFailed, "isn't valid JSON")
	t.Setenv("LINE_LX_TOKEN", "revoked")
	check("refused", hubRefused, "The hub didn't accept your sign-in")
	t.Setenv("LINE_LX_TOKEN", "")
	t.Setenv("LINE_LX_HUB", "")
	if err := saveHub(h.URL); err != nil {
		t.Fatal(err)
	}
	check("signed out", hubSignedOut, "Not signed in")
	// list says the same, and not that the hub didn't answer.
	if out := captureStdout(t, func() { _ = runList() }); !strings.Contains(out, "Not signed in, so this is only what's on this machine.") ||
		strings.Contains(out, "didn't answer") {
		t.Errorf("list, signed out:\n%s", out)
	}
	t.Setenv("LINE_LX_HUB", h.URL)
	t.Setenv("LINE_LX_TOKEN", statusToken)
	h.Close()
	check("unreachable", hubUnreachable, "  The hub didn't answer, so this is only what's on this machine.\n  Can't reach the hub")
}

// TestStatusChangesNothing: status and usage read; every file of the machine is as it was, and nothing is added.
func TestStatusChangesNothing(t *testing.T) {
	h := newStatusHub(t)
	home := statusMachine(t, h.URL)
	snapshot := func() map[string]string {
		files := map[string]string{}
		_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, _ := d.Info()
			sum := ""
			if d.Type().IsRegular() {
				data, _ := os.ReadFile(path)
				sum = fmt.Sprintf("%x", sha256.Sum256(data))
			}
			files[path] = fmt.Sprintf("%v %v %s", info.Mode(), info.ModTime().UnixNano(), sum)
			return nil
		})
		return files
	}
	before := snapshot()
	captureStdout(t, func() { _ = runStatus(false) })
	captureStdout(t, func() { _ = runStatus(true) })
	captureStdout(t, func() { _ = runUsage(false, false) })
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("status changed the machine:\nbefore %v\nafter  %v", before, after)
	}
}
