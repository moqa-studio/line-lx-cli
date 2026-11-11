package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// TestMain keeps every test away from the person's own agents: a home folder of its own, no
// CLAUDE_CONFIG_DIR, so Claude Code's settings are <home>/.claude/settings.json, and no managed settings.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "line-lx-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	claudeManagedDir = filepath.Join(home, "managed")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// fakeHub serves one plugin, basics, and a Claude Code marketplace with the bundle, and records deliveries.
type fakeHub struct {
	*httptest.Server
	mu         sync.Mutex
	version    string   // "": no plugin
	catalog    []string // the marketplace's plugins, but the bundle
	noBundle   bool     // the marketplace lacks the bundle plugin (a hub before it)
	wellKnown  int
	deliveries []map[string]any
}

func newFakeHub(t *testing.T, token string) *fakeHub {
	h := &fakeHub{version: "1.0.0", catalog: []string{"basics", "notes", "tools"}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/.well-known/") {
			h.wellKnown++
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/me":
			w.Write([]byte(`{"user":{"name":"Dev","email":"dev@example.com"},"org":{"name":"Dev","slug":"dev"},"role":"owner","endpoints":{"mcp":"https://mcp.example.com/mcp"}}`))
		case "GET /api/plugins":
			if h.version == "" {
				w.Write([]byte(`[]`))
				return
			}
			w.Write([]byte(`[{"id":"p1","name":"basics"}]`))
		case "GET /api/plugins/p1/versions":
			fmt.Fprintf(w, `[{"id":"v-%s","version":%q,"snapshot":{"skills":[{"name":"greet","content":"hi","files":[]}]}}]`, h.version, h.version)
		case "GET /api/connectors/claude-code/marketplace.json":
			plugins := []any{}
			for _, name := range h.catalog {
				plugins = append(plugins, map[string]any{"name": name})
			}
			if !h.noBundle {
				plugins = append(plugins, map[string]any{"name": bundlePlugin})
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "metro-dev", "owner": map[string]any{"name": "Dev"}, "plugins": plugins})
		case "POST /api/deliveries", "PATCH /api/deliveries/d1":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.deliveries = append(h.deliveries, body)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"d1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

// fakeClaude puts a `claude` on PATH that records its arguments and answers the two list commands
// with the files markets.json and plugins.json. A call whose arguments are in the file fail fails. It
// returns the folder holding them.
func fakeClaude(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
echo "$*" >> '` + bin + `/argv'
if [ -f '` + bin + `/fail' ] && [ "$*" = "$(/bin/cat '` + bin + `/fail')" ]; then echo "boom" >&2; exit 1; fi
case "$*" in
  "--version") echo "2.1.292 (Claude Code)" ;;
  "plugin marketplace list --json") /bin/cat '` + bin + `/markets.json' ;;
  "plugin list --json") /bin/cat '` + bin + `/plugins.json' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setClaude(t, bin, `[]`, `[]`)
	t.Setenv("PATH", bin)
	return bin
}

func setClaude(t *testing.T, bin, markets, plugins string) {
	t.Helper()
	for name, data := range map[string]string{"markets.json": markets, "plugins.json": plugins, "argv": ""} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// claudeEnv gives the test a home of its own, with no ~/.codex or ~/.pi: only Claude Code is found. It
// returns Claude Code's settings file.
func claudeEnv(t *testing.T, hub *fakeHub) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("LINE_LX_HUB", hub.URL)
	t.Setenv("LINE_LX_TOKEN", "tok")
	return filepath.Join(home, ".claude", "settings.json")
}

func writeSettings(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

// readSettings decodes the settings file into plain values, for comparisons.
func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// wantConnected checks the settings declare the hub's marketplace, with the helper, and enable the bundle
// and the plugin basics.
func wantConnected(t *testing.T, s map[string]any, hub string) {
	t.Helper()
	helper, err := helperCommand()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"source":     map[string]any{"source": "url", "url": marketplaceURL(hub), "headersHelper": helper},
		"autoUpdate": true,
	}
	markets, _ := s["extraKnownMarketplaces"].(map[string]any)
	if !reflect.DeepEqual(markets["metro-dev"], want) {
		t.Errorf("extraKnownMarketplaces.metro-dev = %v", markets["metro-dev"])
	}
	if enabled, _ := s["enabledPlugins"].(map[string]any); enabled["metro-lx@metro-dev"] != true || enabled["basics@metro-dev"] != true {
		t.Errorf("enabledPlugins = %v", enabled)
	}
}

func TestSyncConnectsClaudeCode(t *testing.T) {
	hub := newFakeHub(t, "tok")
	bin := fakeClaude(t)
	settings := claudeEnv(t, hub)
	// tools is turned off by the person; gone isn't in the hub anymore, nor off, which the person turned
	// off; the other marketplaces' keys stay, even when they look like ours.
	writeSettings(t, settings, `{"theme":"dark","env":{"A":"1"},"statusLine":{"type":"command","command":"a && b <c>"},
		"enabledPlugins":{"other@elsewhere":false,"gone@elsewhere":true,"tools@metro-dev":false,"gone@metro-dev":true,
		"off@metro-dev":false,"x@metro-dev-2":true,"metro-dev@x":true,"metro-lx@other":true}}`, 0o600)
	hub.mu.Lock()
	hub.catalog = append(hub.catalog, "Bad Name\x1b[31m")
	hub.mu.Unlock()

	// Round 1: Claude Code doesn't know the marketplace. line-lx declares it; Claude Code fetches it when it starts.
	var results []*agentResult
	var err error
	out := captureStdout(t, func() { results, err = syncAll(&hubClient{hub: hub.URL, token: "tok"}) })
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	s := readSettings(t, settings)
	wantConnected(t, s, hub.URL)
	if s["theme"] != "dark" || s["env"].(map[string]any)["A"] != "1" {
		t.Errorf("other settings changed: %v", s)
	}
	wantEnabled := map[string]any{"other@elsewhere": false, "gone@elsewhere": true, "tools@metro-dev": false,
		"off@metro-dev": false, "x@metro-dev-2": true, "metro-dev@x": true, "metro-lx@other": true,
		"basics@metro-dev": true, "notes@metro-dev": true, "metro-lx@metro-dev": true}
	if enabled := s["enabledPlugins"]; !reflect.DeepEqual(enabled, wantEnabled) {
		t.Errorf("enabledPlugins = %v\nwant %v", enabled, wantEnabled)
	}
	if !strings.Contains(out, `claude-code  skipped the plugin "Bad Name[31m": line-lx refuses its name`+"\n") {
		t.Errorf("the refused name isn't shown, or not sanitized:\n%q", out)
	}
	if !strings.Contains(read(t, settings), `"command": "a && b <c>"`) {
		t.Errorf("a setting was escaped:\n%s", read(t, settings))
	}
	if info, _ := os.Stat(settings); info.Mode().Perm() != 0o600 {
		t.Errorf("settings mode = %v, want 0600", info.Mode().Perm())
	}
	if argv := read(t, filepath.Join(bin, "argv")); argv != "plugin marketplace list --json\n" {
		t.Errorf("claude got more than the list:\n%s", argv)
	}
	want := "claude-code  connected: Claude Code gets Dev's plugins from the hub from its next start.\n"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if summary := setupSummary(hub.URL, results); !strings.Contains(summary, "Claude Code  connected: Claude Code gets Dev's plugins") {
		t.Errorf("summary:\n%s", summary)
	}
	if len(hub.deliveries) != 0 {
		t.Errorf("Claude Code deliveries were reported: %v", hub.deliveries)
	}
	if entries, _ := os.ReadDir(filepath.Dir(settings)); len(entries) != 1 {
		t.Errorf("leftovers next to the settings: %v", entries)
	}

	// Round 2: Claude Code knows it now. Nothing to write; line-lx installs the enabled plugins it lacks:
	// not tools, which is off, nor the bundle, which could bring a plugin that's off.
	before := read(t, settings)
	known := `[{"name":"metro-dev","source":"url","url":"` + marketplaceURL(hub.URL) + `"}]`
	setClaude(t, bin, known, `[{"id":"basics@metro-dev","version":"1.0.0"},{"id":"notes@elsewhere","version":"1.0.0"}]`)
	out = captureStdout(t, func() { _, err = syncAll(&hubClient{hub: hub.URL, token: "tok"}) })
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if argv := read(t, filepath.Join(bin, "argv")); !strings.HasSuffix(argv, "plugin list --json\nplugin install -- notes@metro-dev\n") ||
		strings.Contains(argv, "basics@") || strings.Contains(argv, "tools@") || strings.Contains(argv, "metro-lx@") {
		t.Errorf("claude wasn't asked to install the missing plugins only; it got:\n%s", argv)
	}
	if read(t, settings) != before {
		t.Error("an up-to-date sync rewrote the settings")
	}
	if !strings.Contains(out, "claude-code  already connected: Claude Code gets Dev's plugins from the hub by itself. Next: run /reload-plugins") {
		t.Errorf("output:\n%s", out)
	}

	// Round 3: everything is installed: nothing to install.
	setClaude(t, bin, known, `[{"id":"basics@metro-dev"},{"id":"notes@metro-dev"},{"id":"metro-lx@metro-dev"}]`)
	captureStdout(t, func() { _, err = syncAll(&hubClient{hub: hub.URL, token: "tok"}) })
	if argv := read(t, filepath.Join(bin, "argv")); err != nil || strings.Contains(argv, "install") {
		t.Errorf("sync = %v; claude got:\n%s", err, argv)
	}

	// The bundle must be in the hub's marketplace; without it, Claude Code is skipped, not failed.
	hub.mu.Lock()
	hub.noBundle = true
	hub.mu.Unlock()
	setClaude(t, bin, `[]`, `[]`)
	out = captureStdout(t, func() { results, err = syncAll(&hubClient{hub: hub.URL, token: "tok"}) })
	if err != nil || !strings.Contains(out, "claude-code  skipped: "+errNoBundle.Error()) || results[0].skipped == "" {
		t.Errorf("sync = %v:\n%s", err, out)
	}
}

func TestSyncClaudeCodeInstallsWhatItCan(t *testing.T) {
	hub := newFakeHub(t, "tok")
	bin := fakeClaude(t)
	claudeEnv(t, hub)
	run := func() (string, error) {
		var err error
		out := captureStdout(t, func() { _, err = syncAll(&hubClient{hub: hub.URL, token: "tok"}) })
		return out, err
	}
	if out, err := run(); err != nil { // connects; Claude Code knows the marketplace from now on
		t.Fatalf("sync: %v\n%s", err, out)
	}
	known := `[{"name":"metro-dev","source":"url","url":"` + marketplaceURL(hub.URL) + `"}]`
	oldDir, _ := claudeDir(hub.URL)

	// An install that fails doesn't stop the others, nor the cleanup, nor the connection.
	setClaude(t, bin, known, `[]`)
	if err := os.WriteFile(filepath.Join(bin, "fail"), []byte("plugin install -- basics@metro-dev"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	argv := read(t, filepath.Join(bin, "argv"))
	for _, want := range []string{"basics", "notes", "tools"} {
		if !strings.Contains(argv, "plugin install -- "+want+"@metro-dev\n") {
			t.Errorf("%s wasn't installed; claude got:\n%s", want, argv)
		}
	}
	want := "It installs basics at its next start instead (`claude plugin install -- basics@metro-dev`: boom)."
	if err != nil || !strings.Contains(out, want) || exists(oldDir) {
		t.Errorf("sync = %v, former folder left: %v:\n%s", err, exists(oldDir), out)
	}
	os.Remove(filepath.Join(bin, "fail"))

	// Claude Code can't list its plugins: no install, and the next start brings them.
	setClaude(t, bin, known, `not json`)
	out, err = run()
	if argv := read(t, filepath.Join(bin, "argv")); err != nil || strings.Contains(argv, "install") || !strings.Contains(out, "already connected") {
		t.Errorf("sync = %v; claude got:\n%s\n%s", err, argv, out)
	}

	// Past the deadline, line-lx starts no install.
	old := claudeInstallWait
	claudeInstallWait = -time.Second
	t.Cleanup(func() { claudeInstallWait = old })
	setClaude(t, bin, known, `[]`)
	out, err = run()
	if argv := read(t, filepath.Join(bin, "argv")); err != nil || strings.Contains(argv, "install") ||
		!strings.Contains(out, "It installs basics, notes, tools at its next start instead (line-lx stopped installing after") {
		t.Errorf("sync = %v; claude got:\n%s\n%s", err, argv, out)
	}
}

func TestClaudeSettingsKeepPluginsWithAnEmptyCatalog(t *testing.T) {
	hub := newFakeHub(t, "tok")
	hub.catalog = nil // the bundle only: no reason to drop the plugins enabled before
	fakeClaude(t)
	settings := claudeEnv(t, hub)
	writeSettings(t, settings, `{"enabledPlugins":{"basics@metro-dev":true}}`, 0o644)
	captureStdout(t, func() {
		if _, err := syncAll(&hubClient{hub: hub.URL, token: "tok"}); err != nil {
			t.Errorf("sync: %v", err)
		}
	})
	if enabled := readSettings(t, settings)["enabledPlugins"]; !reflect.DeepEqual(enabled, map[string]any{"basics@metro-dev": true, "metro-lx@metro-dev": true}) {
		t.Errorf("enabledPlugins = %v", enabled)
	}
}

func TestSyncClaudeCodeMigratesFromTheFolder(t *testing.T) {
	hub := newFakeHub(t, "tok")
	bin := fakeClaude(t)
	settings := claudeEnv(t, hub)
	oldDir, _ := claudeDir(hub.URL)
	if err := os.MkdirAll(filepath.Join(oldDir, "plugins", "basics"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The folder line-lx used to write, and the hub's marketplace under the workspace's former name.
	writeSettings(t, settings, `{"extraKnownMarketplaces":{
		"metro-dev":{"source":{"source":"directory","path":"`+oldDir+`"}},
		"metro-old":{"source":{"source":"url","url":"`+marketplaceURL(hub.URL)+`"}},
		"acme":{"source":{"source":"github","repo":"acme/plugins"}}},
		"enabledPlugins":{"basics@metro-dev":true,"basics@metro-old":true,"x@acme":true}}`, 0o644)
	setClaude(t, bin, `[{"name":"metro-dev","source":"directory","path":"`+oldDir+`"},
		{"name":"metro-old","source":"url","url":"`+marketplaceURL(hub.URL)+`"},
		{"name":"acme","source":"github"}]`, `[]`)

	out := captureStdout(t, func() {
		if _, err := syncAll(&hubClient{hub: hub.URL, token: "tok"}); err != nil {
			t.Errorf("sync: %v", err)
		}
	})
	argv := read(t, filepath.Join(bin, "argv"))
	for _, want := range []string{"plugin marketplace remove -- metro-dev\n", "plugin marketplace remove -- metro-old\n"} {
		if !strings.Contains(argv, want) {
			t.Errorf("claude wasn't run with %q; it got:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "acme") || strings.Contains(argv, "plugin install") {
		t.Errorf("claude got:\n%s", argv)
	}
	// The fake claude doesn't change the settings: line-lx replaces what's left of the old entries.
	s := readSettings(t, settings)
	wantConnected(t, s, hub.URL)
	markets := s["extraKnownMarketplaces"].(map[string]any)
	enabled := s["enabledPlugins"].(map[string]any)
	if _, ok := markets["metro-old"]; ok || enabled["basics@metro-old"] != nil || markets["acme"] == nil || enabled["x@acme"] != true {
		t.Errorf("settings = %v", s)
	}
	if exists(oldDir) || exists(filepath.Dir(oldDir)) {
		t.Error("line-lx's former folder is still there")
	}
	if !strings.Contains(out, "from its next start. The plugins line-lx installed before come back when Claude Code starts again.") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSyncClaudeCodeWithoutCLI(t *testing.T) {
	hub := newFakeHub(t, "tok")
	settings := claudeEnv(t, hub)
	t.Setenv("PATH", t.TempDir()) // the desktop app or an IDE: ~/.claude, no claude on PATH
	oldDir, _ := claudeDir(hub.URL)
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, settings, `{"extraKnownMarketplaces":{"metro-dev":{"source":{"source":"directory","path":"`+oldDir+`"}}}}`, 0o644)

	out := captureStdout(t, func() {
		if _, err := syncAll(&hubClient{hub: hub.URL, token: "tok"}); err != nil {
			t.Errorf("sync: %v", err)
		}
	})
	wantConnected(t, readSettings(t, settings), hub.URL)
	if !exists(oldDir) {
		t.Error("the former folder went before Claude Code stopped reading it")
	}
	if !strings.Contains(out, "connected: Claude Code gets Dev's plugins from the hub from its next start.") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSyncClaudeCodeLeavesSettingsAlone(t *testing.T) {
	hub := newFakeHub(t, "tok")
	for name, c := range map[string]struct {
		settings, markets string
		skipped           bool
	}{
		"invalid JSON":         {settings: `{"theme":`},
		"not an object":        {settings: `[1, 2]`},
		"markets not object":   {settings: `{"extraKnownMarketplaces":[]}`},
		"enabled not object":   {settings: `{"enabledPlugins":"yes"}`},
		"over the size cap":    {settings: `{"x":"` + strings.Repeat("a", maxSettings) + `"}`},
		"clash in settings":    {settings: `{"extraKnownMarketplaces":{"metro-dev":{"source":{"source":"github","repo":"acme/plugins"}}}}`, skipped: true},
		"clash in Claude Code": {settings: `{}`, markets: `[{"name":"metro-dev","source":"github"}]`, skipped: true},
	} {
		t.Run(name, func(t *testing.T) {
			bin := fakeClaude(t)
			settings := claudeEnv(t, hub)
			writeSettings(t, settings, c.settings, 0o644)
			if c.markets != "" {
				setClaude(t, bin, c.markets, `[]`)
			}
			var results []*agentResult
			var err error
			out := captureStdout(t, func() { results, err = syncAll(&hubClient{hub: hub.URL, token: "tok"}) })
			if c.skipped != (err == nil) || c.skipped != (results[0].skipped != "") {
				t.Errorf("sync = %v, result %+v:\n%s", err, *results[0], out)
			}
			if read(t, settings) != c.settings {
				t.Errorf("the settings changed: %s", read(t, settings))
			}
			if argv := read(t, filepath.Join(bin, "argv")); strings.Contains(argv, "remove") || strings.Contains(argv, "install") {
				t.Errorf("claude got:\n%s", argv)
			}
		})
	}
}

func TestClaudeSettingsThroughSymlink(t *testing.T) {
	hub := newFakeHub(t, "tok")
	fakeClaude(t)
	settings := claudeEnv(t, hub)
	dotfiles := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, dotfiles, `{"theme":"dark"}`, 0o640)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dotfiles, settings); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		if _, err := syncAll(&hubClient{hub: hub.URL, token: "tok"}); err != nil {
			t.Errorf("sync: %v", err)
		}
	})
	if info, err := os.Lstat(settings); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the settings symlink was replaced")
	}
	wantConnected(t, readSettings(t, dotfiles), hub.URL)
	if info, _ := os.Stat(dotfiles); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}

	// A symlink to nothing is refused, and stays.
	if err := os.Remove(dotfiles); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		if _, err := syncAll(&hubClient{hub: hub.URL, token: "tok"}); err == nil {
			t.Error("sync wrote through a symlink to nothing")
		}
	})
	if info, err := os.Lstat(settings); err != nil || info.Mode()&os.ModeSymlink == 0 || exists(dotfiles) {
		t.Error("the dangling settings symlink was replaced or followed")
	}
}

func TestClaudeConnection(t *testing.T) {
	hub := newFakeHub(t, "tok")
	settings := claudeEnv(t, hub)
	exe := filepath.Join(t.TempDir(), "line-lx")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := func(helper, extra string) string {
		return `{"extraKnownMarketplaces":{"metro-dev":{"source":{"source":"url","url":"` + marketplaceURL(hub.URL) +
			`","headersHelper":"` + helper + `"}` + extra + `}}}`
	}
	good := entry(exe+" auth-header", `,"autoUpdate":true`)
	for _, c := range []struct{ data, name, problem string }{
		{`{}`, "", ""},
		{good[:len(good)-1] + `,"enabledPlugins":{"metro-lx@metro-dev":true}}`, "metro-dev", ""},
		{entry("", `,"autoUpdate":true`), "metro-dev", "its marketplace doesn't sign in with `line-lx auth-header`"},
		{entry("/nowhere/line-lx auth-header", `,"autoUpdate":true`), "metro-dev", "its marketplace runs /nowhere/line-lx, which isn't an executable file"},
		{entry(exe+" auth-header", ""), "metro-dev", "its marketplace doesn't auto-update"},
		{good[:len(good)-1] + `,"enabledPlugins":{"metro-lx@metro-dev":false}}`, "metro-dev", "metro-lx@metro-dev isn't enabled"},
	} {
		writeSettings(t, settings, c.data, 0o644)
		if name, where, problem, err := claudeConnection(hub.URL); err != nil || name != c.name || problem != c.problem || name != "" && where != settings {
			t.Errorf("%s: %q, %q, %q, %v", c.data, name, where, problem, err)
		}
	}

	// Managed settings: they declare the marketplace, the person's settings enable the bundle. An unreadable
	// managed file is passed over.
	old := claudeManagedDir
	claudeManagedDir = t.TempDir()
	t.Cleanup(func() { claudeManagedDir = old })
	managed := filepath.Join(claudeManagedDir, "managed-settings.d", "10-metro.json")
	writeSettings(t, managed, good, 0o644)
	writeSettings(t, filepath.Join(claudeManagedDir, "managed-settings.json"), `not json`, 0o644)
	writeSettings(t, settings, `{"enabledPlugins":{"metro-lx@metro-dev":true}}`, 0o644)
	if name, where, problem, err := claudeConnection(hub.URL); err != nil || name != "metro-dev" || where != managed || problem != "" {
		t.Errorf("managed: %q, %q, %q, %v", name, where, problem, err)
	}
	// The managed value wins over the person's.
	writeSettings(t, managed, good[:len(good)-1]+`,"enabledPlugins":{"metro-lx@metro-dev":false}}`, 0o644)
	if _, _, problem, _ := claudeConnection(hub.URL); problem != "metro-lx@metro-dev isn't enabled" {
		t.Errorf("managed false: %q", problem)
	}
}

func TestHelperCommand(t *testing.T) {
	for path, want := range map[string]string{
		"/opt/homebrew/bin/line-lx":           "/opt/homebrew/bin/line-lx auth-header",
		"/home/a_b/Program_Files+1@/line-lx":  "/home/a_b/Program_Files+1@/line-lx auth-header",
		"/Users/me/My Tools/line-lx":          "'/Users/me/My Tools/line-lx' auth-header",
		"/Users/me/it's/line-lx":              "",
		"/Users/me/tab\there/line-lx":         "",
		"/Users/me/é/line-lx":                 "",
		"/Users/me/four    spaces/line-lx":    "",
		"/" + strings.Repeat("a", 500) + "/x": "",
	} {
		got, err := helperFor(path)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("%q: %q, %v", path, got, err)
		}
	}

	// This binary, unless the line-lx on the PATH is this same file: then its path on the PATH.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "line-lx"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	same := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(same, "line-lx")); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{other: exe, same: filepath.Join(same, "line-lx")} {
		t.Setenv("PATH", dir)
		wantCmd, _ := helperFor(want)
		if got, err := helperCommand(); err != nil || got != wantCmd {
			t.Errorf("PATH %s: %q, %v; want %q", dir, got, err, wantCmd)
		}
	}
}

func TestClaudeVersionOK(t *testing.T) {
	for out, want := range map[string]bool{
		"2.1.292 (Claude Code)\n": true,
		"2.1.238 (Claude Code)":   true,
		"2.1.300-beta.1":          true,
		"2.1.238+build.5":         true,
		"2.2.0":                   true,
		"3.0.0":                   true,
		"2.1.237 (Claude Code)":   false,
		"2.1.237-rc.1":            false,
		"2.0.999":                 false,
		"1.9.300":                 false,
		"2.1":                     false,
		"":                        false,
		"garbage":                 false,
	} {
		if _, got := claudeVersionOK(out); got != want {
			t.Errorf("%q: %v", out, got)
		}
	}
}

func TestSetupSkipsSignInWhenSignedIn(t *testing.T) {
	keyring.MockInit()
	hub := newFakeHub(t, "tok")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PATH", t.TempDir()) // no claude
	t.Setenv("LINE_LX_HUB", "")
	t.Setenv("LINE_LX_TOKEN", "")
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	hubURL, _ := checkHub(hub.URL)
	if err := saveCreds(hubURL, &creds{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour), Resource: hubURL + "/api"}); err != nil {
		t.Fatal(err)
	}

	for range 2 { // twice: running it again is safe
		if err := runSetup(hub.URL); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	if hub.wellKnown != 0 {
		t.Error("setup started a sign-in although the person is signed in")
	}
	if read(t, filepath.Join(home, ".pi", "agent", "skills", "basics", "greet", "SKILL.md")) != "hi" {
		t.Error("setup didn't sync")
	}
	if got, _ := currentHub(); got != hubURL {
		t.Errorf("setup didn't remember the hub: %q", got)
	}
}

func TestSetupSignsInWhenNeeded(t *testing.T) {
	keyring.MockInit()
	hub := newFakeHub(t, "tok")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LINE_LX_HUB", "")
	t.Setenv("LINE_LX_TOKEN", "")
	hubURL, _ := checkHub(hub.URL)

	// No sign-in, one the hub refuses, then one from an earlier line-lx: each time setup goes to the hub's
	// OAuth (which this fake hub doesn't have, so the sign-in fails there).
	for _, c := range []*creds{
		nil,
		{AccessToken: "revoked", ExpiresAt: time.Now().Add(time.Hour), Resource: hubURL + "/api"},
		{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if c != nil {
			if err := saveCreds(hubURL, c); err != nil {
				t.Fatal(err)
			}
		}
		before := hub.wellKnown
		if err := runSetup(hub.URL); err == nil || hub.wellKnown == before {
			t.Errorf("creds %v: setup = %v without starting a sign-in", c, err)
		}
	}

	// LINE_LX_TOKEN is CI's: a refused one is an error, never a browser sign-in.
	t.Setenv("LINE_LX_HUB", hub.URL)
	t.Setenv("LINE_LX_TOKEN", "revoked")
	before := hub.wellKnown
	if err := runSetup(""); !errors.As(err, new(errSignInRefused)) || hub.wellKnown != before {
		t.Errorf("setup with a refused LINE_LX_TOKEN = %v, sign-in started: %v", err, hub.wellKnown != before)
	}
}

func TestBearerKeepsTokenOnItsHub(t *testing.T) {
	t.Setenv("LINE_LX_TOKEN", "tok")
	t.Setenv("LINE_LX_HUB", "https://hub.example.com/")
	if tok, err := bearer("https://hub.example.com"); err != nil || tok != "tok" {
		t.Errorf("bearer for LINE_LX_HUB = %q, %v", tok, err)
	}
	if _, err := bearer("https://other.example.com"); err == nil {
		t.Error("LINE_LX_TOKEN went to another hub")
	}
}

func TestSetupSummary(t *testing.T) {
	home, _ := os.UserHomeDir()
	got := setupSummary("https://hub.example.com", []*agentResult{
		{name: claudeTarget, connected: "connected: Claude Code gets Dev's plugins from the hub from its next start."},
		{name: "pi", where: filepath.Join(home, ".pi", "agent", "skills"), current: 3},
		{name: "codex", where: "/x", delivered: 1, failed: 1},
		{name: claudeTarget, skipped: "the hub has no Claude Code marketplace yet"},
		{name: claudeTarget, failed: 1},
	})
	for _, want := range []string{
		"Claude Code  connected: Claude Code gets Dev's plugins from the hub from its next start.\n",
		"pi           3 plugins in ~/.pi/agent/skills. Nothing new.\n", // no next step
		"Codex        1 plugin in /x. 1 failed: see above, or run `line-lx doctor`. Next: start a new Codex session.\n",
		"Claude Code  nothing installed: the hub has no Claude Code marketplace yet.\n",
		"Claude Code  not connected: see above, or run `line-lx doctor`.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}

// captureStdout returns what fn prints.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	out := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()
	defer func() { os.Stdout = stdout }()
	fn()
	w.Close()
	return <-out
}

func TestSyncClaudeCodeFailure(t *testing.T) {
	hub := newFakeHub(t, "tok")
	bin := fakeClaude(t)
	claudeEnv(t, hub)
	setClaude(t, bin, `not json`, `[]`) // claude can't list its marketplaces

	var results []*agentResult
	var err error
	out := captureStdout(t, func() { results, err = syncAll(&hubClient{hub: hub.URL, token: "tok"}) })
	if !errors.As(err, new(errSilent)) {
		t.Fatalf("sync = %v, want a failure", err)
	}
	if !strings.Contains(out, "claude-code  failed: can't read `claude plugin marketplace list --json`") || !strings.Contains(out, "1 failed.") {
		t.Errorf("the error isn't shown:\n%s", out)
	}
	if summary := setupSummary(hub.URL, results); !strings.Contains(summary, "Claude Code  not connected: see above") {
		t.Errorf("summary:\n%s", summary)
	}
}

func TestClaudeTimesOut(t *testing.T) {
	bin := fakeClaude(t)
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := claudeTimeout
	claudeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { claudeTimeout = old })

	start := time.Now()
	_, err := claude("plugin", "list", "--json")
	if err == nil || !strings.Contains(err.Error(), "`claude plugin list --json` didn't finish within 300ms, so line-lx stopped it") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("line-lx waited %v for a stalled claude", time.Since(start))
	}
	if durationText(2*time.Minute) != "2 minutes" {
		t.Errorf("durationText = %q", durationText(2*time.Minute))
	}
}

func TestSyncLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	old := lockWait
	lockWait = 300 * time.Millisecond
	t.Cleanup(func() { lockWait = old })

	unlock, err := syncLock()
	if err != nil {
		t.Fatal(err)
	}
	var second error
	out := captureStdout(t, func() { _, second = syncLock() })
	if second == nil || !strings.Contains(second.Error(), "Another line-lx is syncing this machine") || !strings.Contains(out, "Waiting for another line-lx") {
		t.Errorf("a second sync while one runs = %v, printed %q", second, out)
	}
	unlock()
	unlock, err = syncLock()
	if err != nil {
		t.Fatalf("the lock wasn't released: %v", err)
	}
	unlock()

	// While one sync runs, a second waits for it instead of clearing the staging folders under it.
	lockWait = 5 * time.Second
	hub := newFakeHub(t, "tok")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // no claude
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if unlock, err = syncLock(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	captureStdout(t, func() {
		go func() {
			_, err := syncAll(&hubClient{hub: hub.URL, token: "tok"})
			done <- err
		}()
		select {
		case err := <-done:
			t.Errorf("a sync ran while another held the lock: %v", err)
			return
		case <-time.After(500 * time.Millisecond):
		}
		unlock()
		if err := <-done; err != nil {
			t.Errorf("the waiting sync failed: %v", err)
		}
	})
	if read(t, filepath.Join(home, ".pi", "agent", "skills", "basics", "greet", "SKILL.md")) != "hi" {
		t.Error("the waiting sync didn't write")
	}
}

func TestSetupWithoutAgentGivesTheHubsMCPURL(t *testing.T) {
	keyring.MockInit()
	hub := newFakeHub(t, "tok")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PATH", t.TempDir()) // no claude
	t.Setenv("LINE_LX_HUB", hub.URL)
	t.Setenv("LINE_LX_TOKEN", "tok")
	var err error
	out := captureStdout(t, func() { err = runSetup("") })
	if err != nil || !strings.Contains(out, "connect to the MCP server instead: https://mcp.example.com/mcp\n") {
		t.Errorf("setup = %v, printed:\n%s", err, out)
	}
}
