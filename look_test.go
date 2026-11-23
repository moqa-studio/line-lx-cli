package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
)

// sgr is a color or a style, as painter writes them: what a golden's colored output loses to read as the plain one.
var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

// inColor renders a screen at every depth, and checks a color only adds to what the text says: without its escape
// sequences, the colored output is the plain one, but for the badges, " 1 " on the line's color for "(1)". Every line
// fits in 80 columns. It returns the plain output.
func inColor(t *testing.T, render func(w io.Writer, c painter)) string {
	t.Helper()
	var bare strings.Builder
	render(&bare, noColor)
	want := regexp.MustCompile(`\((\d)\)`).ReplaceAllString(bare.String(), " $1 ")
	for _, depth := range []painter{colors256, trueColor} {
		var colored strings.Builder
		render(&colored, depth)
		if got := sgr.ReplaceAllString(colored.String(), ""); got != want {
			t.Errorf("depth %d, without its colors:\n%s\nwant:\n%s", depth, got, want)
		}
	}
	for i, line := range strings.Split(bare.String(), "\n") {
		if n := cols(line); n > screenWidth {
			t.Errorf("line %d is %d columns wide: %q", i+1, n, line)
		}
	}
	return bare.String()
}

func golden(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n%s\nwant:\n%s", name, got, want)
	}
}

// statusGolden is the fixture machine, side by side with the hub, at statusNow, without color.
const statusGolden = `
  ▌▌▌ metro-lx  Dev                                         signed in as Robin
      hub  HUB

  ━───────  2 of 11 up to date · 3 behind · 5 not installed · 1 not in the hub

  ╭─ Claude Code ─────────────────────────────── connected · fetched 2h ago ─╮
  │                                                                          │
  │  (1)  notes      v1.2.0                     up to date        yesterday  │
  │        ●  jot                               ✓ No findings                │
  │        ●  summary                           ! To review · 1 medium       │
  │                                                                          │
  │  (2)  tools      v1.0.0 → v1.1.0            behind          10 Nov 2026  │
  │        ●  lint                              · No static findings         │
  │                                                                          │
  │  (3)  extra      —                          not installed             —  │
  │        ●  help                              ✗ Flagged · 1 critical       │
  │                                                                          │
  │   ◎   metro-lx   13cd9935af1d → a1b2c3d4e5f6   behind       12 Nov 2026  │
  │        ▲  PostToolUse                       runs line-lx report-usage…   │
  │        ▲  PostToolUseFailure                runs line-lx report-usage…   │
  │                                                                          │
  │  Run ` + "`" + `line-lx sync` + "`" + `, or start Claude Code, to install what's missing.    │
  │                                                                          │
  ╰──────────────────────────────────────────────────────────────────────────╯

  ╭─ Codex ───────────────────────────────────────── set up · synced 2d ago ─╮
  │                                                                          │
  │  (1)  notes      v1.1.0 → v1.2.0            behind               2d ago  │
  │        ●  jot                               ✓ No findings                │
  │                                                                          │
  │  (2)  tools      —                          not installed             —  │
  │        ●  lint                              · No static findings         │
  │                                                                          │
  │  (3)  extra      —                          not installed             —  │
  │        ●  help                              ✗ Flagged · 1 critical       │
  │                                                                          │
  │       old-notes  v2.0.0                     not in the hub   1 Sep 2026  │
  │        ●  gone                                                           │
  │                                                                          │
  │  Run ` + "`" + `line-lx sync` + "`" + ` to catch up with the hub.                            │
  │                                                                          │
  ╰──────────────────────────────────────────────────────────────────────────╯

  ╭─ pi ──────────────────────────────────────────── set up · synced 2m ago ─╮
  │                                                                          │
  │  (1)  notes      v1.2.0                     up to date           2m ago  │
  │        ●  jot                               ✓ No findings                │
  │        ●  summary                           ! To review · 1 medium       │
  │                                                                          │
  │  (2)  tools      —                          not installed             —  │
  │        ●  lint                              · No static findings         │
  │                                                                          │
  │  (3)  extra      —                          not installed             —  │
  │        ●  help                              ✗ Flagged · 1 critical       │
  │                                                                          │
  │       theirs     v3.0.0                     other hub            1h ago  │
  │           from https://other.example.com                                 │
  │        ●  help-them                                                      │
  │                                                                          │
  │  Run ` + "`" + `line-lx sync` + "`" + ` to catch up with the hub.                            │
  │                                                                          │
  ╰──────────────────────────────────────────────────────────────────────────╯
`

// TestStatusGolden reads the fixture machine (Claude Code's records, line-lx's stamps), puts it side by side with the
// hub, and renders it.
func TestStatusGolden(t *testing.T) {
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	hub, _ := checkHub(h.URL)
	r := buildStatus(hub, fetchHubView(hub))
	got := inColor(t, func(w io.Writer, c painter) { renderStatus(w, r, statusNow, c) })
	golden(t, "status", got, strings.ReplaceAll(statusGolden, "HUB", hub))
}

const usageGolden = `
  ▌▌▌ metro-lx  Your runs · last 7 days                                53 runs

  (1)  notes             ████████████████████████████████████████   35

  (2)  tools             ████████████████████▋                      18

  BY AGENT

       Claude Code       ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━   49
       Codex             ━━━                                         4

  Only your own runs. Admins see the workspace's totals in the web app.
  Run ` + "`" + `line-lx usage --month` + "`" + ` for the last 30 days.
`

// fixtureRuns is the usage fixture, counted, with the lines of the status fixture's hub.
func fixtureRuns(t *testing.T, period, answer string, reporting bool) runsReport {
	t.Helper()
	var mine myUsage
	var installed hubInstalled
	if err := json.Unmarshal([]byte(answer), &mine); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(statusAnswer("https://hub.example.com")), &installed); err != nil {
		t.Fatal(err)
	}
	return countRuns("https://hub.example.com", period, mine, readHubView(installed), &reporting)
}

// TestUsageGolden counts runs as the web app does (an agent's report that a skill ran), and draws them.
func TestUsageGolden(t *testing.T) {
	r := fixtureRuns(t, "week", usageAnswer, true)
	golden(t, "usage", inColor(t, func(w io.Writer, c painter) { renderUsage(w, r, c) }), usageGolden)

	// No runs: the web app's words, and why when agent reporting is off.
	empty := `{"since": "2026-11-13", "rows": []}`
	for reporting, want := range map[bool]string{true: "Nothing counted for you yet.", false: "Agent reporting is off in this workspace"} {
		var b strings.Builder
		renderUsage(&b, fixtureRuns(t, "month", empty, reporting), noColor)
		if !strings.Contains(b.String(), want) || !strings.Contains(b.String(), "Your runs · last 30 days") {
			t.Errorf("reporting %v:\n%s", reporting, b.String())
		}
	}
}

const doctorGolden = `
  ▌▌▌ metro-lx  doctor

  ✓  Hub           hub.example.com · reachable
  ✗  Sign-in       from an earlier line-lx
                   → Run ` + "`" + `line-lx login` + "`" + `.
  ✓  Claude Code   connected · ~/.claude/settings.json
  !  Claude Code   2.1.311 is older than 2.1.319
                   → Run ` + "`" + `claude update` + "`" + `, or new plugins won't arrive.
  ·  Codex         not found
  ✓  pi            ~/.pi/agent/skills · 2 of 2 plugins up to date
                   pi reads skills on startup: restart it to pick up changes

  1 problem · 1 warning
`

func TestDoctorGolden(t *testing.T) {
	checks := []doctorCheck{
		{kind: checkOK, label: "Hub", text: "hub.example.com", more: " · reachable"},
		{kind: checkFail, label: "Sign-in", text: "from an earlier line-lx", fix: "Run `line-lx login`."},
		{kind: checkOK, label: "Claude Code", text: "connected", more: " · ~/.claude/settings.json"},
		{kind: checkWarn, label: "Claude Code", text: outdatedText("2.1.311"), fix: claudeUpdate},
		{kind: checkNone, label: "Codex", text: "not found"},
		{kind: checkOK, label: "pi", text: "~/.pi/agent/skills", more: " · 2 of 2 plugins up to date",
			note: "pi reads skills on startup: restart it to pick up changes"},
	}
	golden(t, "doctor", inColor(t, func(w io.Writer, c painter) { renderDoctor(w, checks, c) }), doctorGolden)
}

const syncGolden = `
  ▌▌▌ metro-lx  sync · Dev

  ✓  Claude Code   connected · it installs new versions itself
  ✓  Codex         3 plugins · ~/.codex/skills
                   (1)  notes             v1.2.0   added
                   (2)  tools             v1.1.0   updated from v1.0.0
                   → Start a new Codex session to load them.
  ·  pi            not found

  Done in 1.4s.
`

// TestSyncGolden: what sync prints for each agent, from what it did.
func TestSyncGolden(t *testing.T) {
	home, _ := os.UserHomeDir()
	results := []*agentResult{
		{name: claudeTarget, claude: &claudeConnected{now: true}},
		{name: "codex", where: filepath.Join(home, ".codex", "skills"), delivered: 2, current: 1, changes: []pluginChange{
			{name: "notes", version: "1.2.0", action: "added"},
			{name: "tools", version: "1.1.0", from: "1.0.0", action: "updated"},
		}},
	}
	lines := map[string]string{"notes": "engineering", "tools": "product"}
	got := inColor(t, func(w io.Writer, c painter) {
		writeHeader(w, c, []seg{plain("sync · Dev")}, nil)
		io.WriteString(w, "\n")
		for _, r := range results {
			for _, line := range syncRows(r, lines, c) {
				io.WriteString(w, line+"\n")
			}
		}
		io.WriteString(w, strings.Join(checkRow(c, checkNone, "pi", seg{"not found", c.dim}), "\n")+"\n\n")
		io.WriteString(w, doneRow(0, 1400*time.Millisecond, c)+"\n")
	})
	golden(t, "sync", got, syncGolden)
}

// TestHostileTextPrintsNoControl: a review label or a usage plugin name from the hub with escape sequences (CSI, OSC),
// C1 controls and bidi overrides reaches the terminal as plain text, whatever the color depth.
func TestHostileTextPrintsNoControl(t *testing.T) {
	const evil = "\u001b[2J\u001b]8;;http://evil.example\u0007\u009b31m\u0090\u202e"
	var installed hubInstalled
	if err := json.Unmarshal([]byte(`{"workspace": {"slug": "dev", "name": "Dev"}, "agents": [{"target": "codex", "plugins": [
  {"name": "notes", "route": 1, "line": "engineering", "latestVersion": "1.0.0",
   "skills": [{"name": "jot", "review": {"status": "done", "label": "To`+jsonText(evil)+` review", "tone": "attention", "counts": {"medium": 1}}}]}]}]}`), &installed); err != nil {
		t.Fatal(err)
	}
	hv := readHubView(installed)
	r := statusReport{Hub: "https://hub.example.com", HubStatus: hubOK, Workspace: hv.workspace, Agents: []statusAgent{{
		Target: "codex", Found: true, Connected: true,
		Plugins: mergePlugins("codex", "https://hub.example.com", map[string]localPlugin{}, hv), // not here: the hub's skills, with their reviews,
	}}}
	var mine myUsage
	if err := json.Unmarshal([]byte(`{"since": "2026-11-13", "rows": [{"plugin": "no`+jsonText(evil)+`tes", "agent": "co`+jsonText(evil)+`dex", "source": "agent", "kind": "activated", "uses": 3}]}`), &mine); err != nil {
		t.Fatal(err)
	}
	runs := countRuns("https://hub.example.com", "week", mine, hv, nil)

	for _, c := range []painter{noColor, colors256, trueColor} {
		var b strings.Builder
		renderStatus(&b, r, statusNow, c)
		renderUsage(&b, runs, c)
		out := b.String()
		if c != noColor {
			out = sgr.ReplaceAllString(out, "")
		}
		for _, ch := range out {
			if ch != '\n' && (unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch)) {
				t.Errorf("depth %d: %U in the output:\n%q", c, ch, out)
				break
			}
		}
		for _, want := range []string{"To review · 1 medium", "notes", "Codex"} {
			if !strings.Contains(out, want) {
				t.Errorf("depth %d: %q isn't in the output:\n%s", c, want, out)
			}
		}
	}
}

// jsonText is s as it goes inside a JSON string.
func jsonText(s string) string {
	data, _ := json.Marshal(s)
	return string(data[1 : len(data)-1])
}

// keys are an object's keys, sorted.
func keys(v any) []string {
	m, _ := v.(map[string]any)
	return sortedKeys(m)
}

// TestStatusJSONContract: status --json keeps schema 1's fields, and gives each skill its review, null without one.
func TestStatusJSONContract(t *testing.T) {
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	var doc map[string]any
	if err := json.Unmarshal([]byte(captureStdout(t, func() { _ = runStatus(true) })), &doc); err != nil {
		t.Fatal(err)
	}
	if got, want := keys(doc), []string{"agents", "hub", "hubStatus", "schema", "workspace"}; !slices.Equal(got, want) || doc["schema"] != 1.0 {
		t.Errorf("status = %v, schema %v", got, doc["schema"])
	}
	claude := doc["agents"].([]any)[0].(map[string]any)
	if got, want := keys(claude), []string{"connected", "found", "location", "marketplace", "plugins", "target"}; !slices.Equal(got, want) {
		t.Errorf("agent = %v", got)
	}
	notes := claude["plugins"].([]any)[0].(map[string]any)
	want := []string{"behind", "bundle", "deliveredVersion", "firstDeliveredAt", "hooks", "installedAt", "lastDeliveredAt", "latestVersion",
		"line", "name", "route", "scope", "skills", "state", "updatedAt", "version"}
	if got := keys(notes); !slices.Equal(got, want) {
		t.Errorf("plugin = %v\nwant %v", got, want)
	}
	summary := notes["skills"].([]any)[1].(map[string]any)
	review := summary["review"].(map[string]any)
	if got := keys(summary); !slices.Equal(got, []string{"description", "name", "review"}) {
		t.Errorf("skill = %v", got)
	}
	if got := keys(review); !slices.Equal(got, []string{"counts", "label", "status", "tone"}) || review["label"] != "To review" || review["tone"] != "attention" {
		t.Errorf("review = %v", review)
	}
	if got := keys(review["counts"]); !slices.Equal(got, []string{"critical", "high", "low", "medium"}) {
		t.Errorf("counts = %v", got)
	}
	// The bundle's hooks, and a skill the hub has no review for: null, not missing.
	gone := doc["agents"].([]any)[1].(map[string]any)["plugins"].([]any)[3].(map[string]any)["skills"].([]any)[0].(map[string]any)
	if r, ok := gone["review"]; !ok || r != nil {
		t.Errorf("a skill without a review = %v", gone)
	}
}

// TestUsageJSONContract: usage --json asks the hub for the period, and counts runs as the web app does.
func TestUsageJSONContract(t *testing.T) {
	h := newStatusHub(t)
	statusMachine(t, h.URL)
	for flag, period := range map[bool]string{false: "week", true: "month"} {
		var doc map[string]any
		if err := json.Unmarshal([]byte(captureStdout(t, func() { _ = runUsage(flag, true) })), &doc); err != nil {
			t.Fatal(err)
		}
		if got, want := keys(doc), []string{"agentReporting", "agents", "hub", "period", "plugins", "runs", "schema", "since"}; !slices.Equal(got, want) {
			t.Errorf("usage = %v", got)
		}
		if doc["schema"] != 1.0 || doc["period"] != period || doc["runs"] != 53.0 || doc["since"] != "2026-11-13" || doc["agentReporting"] != true {
			t.Errorf("usage = %v", doc)
		}
		notes := doc["plugins"].([]any)[0].(map[string]any)
		if got := keys(notes); !slices.Equal(got, []string{"line", "name", "route", "runs"}) || notes["name"] != "notes" || notes["route"] != 1.0 || notes["runs"] != 35.0 {
			t.Errorf("plugin = %v", notes)
		}
		claude := doc["agents"].([]any)[0].(map[string]any)
		if got := keys(claude); !slices.Equal(got, []string{"agent", "name", "runs"}) || claude["name"] != "Claude Code" || claude["runs"] != 49.0 {
			t.Errorf("agent = %v", claude)
		}
		h.mu.Lock()
		asked := strings.Join(h.requests, "\n")
		h.mu.Unlock()
		if !strings.Contains(asked, "GET /api/usage/mine?period="+period+" ") {
			t.Errorf("requests:\n%s", asked)
		}
	}
}
