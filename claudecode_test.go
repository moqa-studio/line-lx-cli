package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

type zipEntry struct {
	name, body string
	mode       os.FileMode // 0: a plain file
}

func makeZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func manifest(name, version string) zipEntry {
	return zipEntry{name: ".claude-plugin/plugin.json", body: fmt.Sprintf(`{"name":%q,"version":%q}`, name, version)}
}

func TestExtractPlugin(t *testing.T) {
	dir := t.TempDir()
	good := makeZip(t, manifest("demo", "1.0.0"), zipEntry{name: "skills/greet/"}, zipEntry{name: "skills/greet/SKILL.md", body: "hi"})
	if err := extractPlugin(good, filepath.Join(dir, "ok"), "demo"); err != nil {
		t.Fatalf("a good archive was refused: %v", err)
	}
	if read(t, filepath.Join(dir, "ok", "skills", "greet", "SKILL.md")) != "hi" {
		t.Error("the skill wasn't extracted")
	}

	big := strings.Repeat("0", maxArchiveEntry+1)
	many := []zipEntry{manifest("demo", "1.0.0")}
	for i := range maxArchiveEntries {
		many = append(many, zipEntry{name: fmt.Sprintf("f%d", i)})
	}
	total := []zipEntry{manifest("demo", "1.0.0")}
	for i := range maxArchiveTotal/maxArchiveEntry + 1 {
		total = append(total, zipEntry{name: fmt.Sprintf("f%d", i), body: big[:maxArchiveEntry]})
	}
	for name, entries := range map[string][]zipEntry{
		"absolute path":       {manifest("demo", "1"), {name: "/etc/evil"}},
		"dot dot":             {manifest("demo", "1"), {name: "../evil"}},
		"dot dot inside":      {manifest("demo", "1"), {name: "skills/../../evil"}},
		"backslash":           {manifest("demo", "1"), {name: `..\evil`}},
		"drive letter":        {manifest("demo", "1"), {name: "C:evil"}},
		"empty segment":       {manifest("demo", "1"), {name: "a//b"}},
		"symlink":             {manifest("demo", "1"), {name: "link", body: "/etc/passwd", mode: os.ModeSymlink | 0o777}},
		"device":              {manifest("demo", "1"), {name: "dev", mode: os.ModeDevice | 0o644}},
		"duplicate":           {manifest("demo", "1"), {name: "a", body: "1"}, {name: "a", body: "2"}},
		"duplicate by case":   {manifest("demo", "1"), {name: "a", body: "1"}, {name: "A", body: "2"}},
		"file and folder":     {manifest("demo", "1"), {name: "a", body: "1"}, {name: "a/"}},
		"entry over the cap":  {manifest("demo", "1"), {name: "big", body: big}},
		"total over the cap":  total,
		"too many entries":    many,
		"no manifest":         {{name: "skills/greet/SKILL.md", body: "hi"}},
		"another's manifest":  {manifest("other", "1")},
		"unreadable manifest": {{name: ".claude-plugin/plugin.json", body: "{"}},
	} {
		dest := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := extractPlugin(makeZip(t, entries...), dest, "demo"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if exists(filepath.Join(dir, "evil")) || exists(filepath.Join(filepath.Dir(dir), "evil")) {
		t.Error("an entry landed outside the plugin's folder")
	}
	if err := extractPlugin([]byte("not a zip"), filepath.Join(dir, "garbage"), "demo"); err == nil {
		t.Error("garbage was accepted as an archive")
	}
}

func TestDownloadStaysOnTheHub(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere++ }))
	defer other.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("zip"))
	}))
	defer hub.Close()

	c := &hubClient{hub: hub.URL + "/hub", token: "tok"}
	if data, err := c.download(hub.URL+"/hub/api/x.zip", 10); err != nil || string(data) != "zip" {
		t.Fatalf("download on the hub = %q, %v", data, err)
	}
	for _, link := range []string{
		other.URL + "/hub/api/x.zip",
		hub.URL + "/api/x.zip", // outside the hub's path
		hub.URL + "/hub/../api/x.zip",
		strings.Replace(hub.URL, "http://", "http://user@", 1) + "/hub/api/x.zip",
		"file:///etc/passwd",
	} {
		if _, err := c.download(link, 10); err == nil {
			t.Errorf("%s: downloaded", link)
		}
	}
	if elsewhere != 0 {
		t.Error("the token went to another host")
	}
	if _, err := c.download(hub.URL+"/hub/api/x.zip", 2); err == nil {
		t.Error("a file over the limit was accepted")
	}
}

// fakeHub serves one plugin, basics, its Claude Code marketplace and archive, and records deliveries.
type fakeHub struct {
	*httptest.Server
	mu          sync.Mutex
	version     string // "": no plugin
	sha         string // the sha256 the marketplace lists; "" for the archive's own
	archiveHits int
	wellKnown   int
	deliveries  []map[string]any
}

func (h *fakeHub) archive(version string) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range []zipEntry{manifest("basics", version), {name: "skills/greet/SKILL.md", body: "hi " + version}} {
		f, _ := w.Create(e.name)
		f.Write([]byte(e.body))
	}
	w.Close()
	return buf.Bytes()
}

func newFakeHub(t *testing.T, token string) *fakeHub {
	h := &fakeHub{version: "1.0.0"}
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
		zipPath := "/api/connectors/claude-code/plugins/basics/" + h.version + "/bundle.zip"
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
			if h.version != "" {
				sum := sha256.Sum256(h.archive(h.version))
				sha := hex.EncodeToString(sum[:])
				if h.sha != "" {
					sha = h.sha
				}
				plugins = append(plugins, map[string]any{"name": "basics", "description": "B", "category": "tools",
					"source": map[string]any{"source": "archive", "url": h.URL + zipPath, "sha256": sha}})
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "metro-dev", "owner": map[string]any{"name": "Dev"}, "metadata": map[string]any{"x": 1}, "plugins": plugins})
		case "GET " + zipPath:
			h.archiveHits++
			w.Write(h.archive(h.version))
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
// with the files markets.json and plugins.json. It returns the folder holding them.
func fakeClaude(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
echo "$*" >> '` + bin + `/argv'
case "$*" in
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

func claudeEnv(t *testing.T, hub *fakeHub) {
	t.Setenv("HOME", t.TempDir()) // no ~/.codex or ~/.pi: only Claude Code is found
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LINE_LX_HUB", hub.URL)
	t.Setenv("LINE_LX_TOKEN", "tok")
}

func TestSyncClaudeCode(t *testing.T) {
	hub := newFakeHub(t, "tok")
	bin := fakeClaude(t)
	claudeEnv(t, hub)
	dir, err := claudeDir(hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Round 1: Claude Code has the hub's marketplace by URL, and a plugin the hub dropped.
	setClaude(t, bin, `[{"name":"metro-dev","source":"url","url":"`+marketplaceURL(hub.URL)+`"}]`, `[{"id":"gone@metro-dev","version":"0.1.0"}]`)
	if err := runSync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	argv := read(t, filepath.Join(bin, "argv"))
	remove, add := strings.Index(argv, "plugin marketplace remove -- metro-dev\n"), strings.Index(argv, "plugin marketplace add -- "+dir+"\n")
	if remove < 0 || add < remove {
		t.Errorf("want the URL marketplace removed, then the folder added; claude got:\n%s", argv)
	}
	for _, want := range []string{"plugin install -- basics@metro-dev", "plugin uninstall -- gone@metro-dev"} {
		if !strings.Contains(argv, want+"\n") {
			t.Errorf("claude wasn't run with %q; it got:\n%s", want, argv)
		}
	}
	if got := read(t, filepath.Join(dir, "plugins", "basics", "skills", "greet", "SKILL.md")); got != "hi 1.0.0" {
		t.Errorf("SKILL.md = %q", got)
	}
	// The marketplace is the hub's, with each source pointing at the plugin's folder: nothing else changes.
	var got, want map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"))), &got); err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(func() *http.Request {
		r, _ := http.NewRequest("GET", hub.URL+"/api/connectors/claude-code/marketplace.json", nil)
		r.Header.Set("Authorization", "Bearer tok")
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(res.Body).Decode(&want)
	res.Body.Close()
	want["plugins"].([]any)[0].(map[string]any)["source"] = "./plugins/basics"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("marketplace.json = %v\nwant %v", got, want)
	}
	if len(hub.deliveries) != 1 || hub.deliveries[0]["status"] != "delivered" || hub.deliveries[0]["target"] != "claude-code" {
		t.Errorf("deliveries = %v", hub.deliveries)
	}

	// Round 2: nothing new. The catalog is refreshed, nothing downloaded or installed.
	ours := `[{"name":"metro-dev","source":"directory","path":"` + dir + `"}]`
	setClaude(t, bin, ours, `[{"id":"basics@metro-dev","version":"1.0.0"}]`)
	hits := hub.archiveHits
	if err := runSync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	argv = read(t, filepath.Join(bin, "argv"))
	if !strings.Contains(argv, "plugin marketplace update -- metro-dev\n") || strings.Contains(argv, "plugin install") ||
		strings.Contains(argv, "plugin update") || strings.Contains(argv, "marketplace add") || hub.archiveHits != hits {
		t.Errorf("an up-to-date sync did more than refresh the catalog (%d downloads); claude got:\n%s", hub.archiveHits-hits, argv)
	}

	// Round 3: a new version is written to the folder, then Claude Code updates.
	hub.version = "1.1.0"
	setClaude(t, bin, ours, `[{"id":"basics@metro-dev","version":"1.0.0"}]`)
	if err := runSync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if argv := read(t, filepath.Join(bin, "argv")); !strings.Contains(argv, "plugin update -- basics@metro-dev\n") {
		t.Errorf("claude wasn't asked to update; it got:\n%s", argv)
	}
	if got := read(t, filepath.Join(dir, "plugins", "basics", "skills", "greet", "SKILL.md")); got != "hi 1.1.0" {
		t.Errorf("SKILL.md = %q after the new version", got)
	}

	// Round 4: the hub dropped the plugin. It's uninstalled, and its folder goes.
	hub.version = ""
	setClaude(t, bin, ours, `[{"id":"basics@metro-dev","version":"1.1.0"}]`)
	if err := runSync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if argv := read(t, filepath.Join(bin, "argv")); !strings.Contains(argv, "plugin uninstall -- basics@metro-dev\n") {
		t.Errorf("claude wasn't asked to uninstall; it got:\n%s", argv)
	}
	if exists(filepath.Join(dir, "plugins", "basics")) {
		t.Error("the dropped plugin's folder is still there")
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 1 {
		t.Errorf("leftovers next to the marketplace: %v", entries)
	}
}

func TestSyncClaudeCodeShaMismatch(t *testing.T) {
	hub := newFakeHub(t, "tok")
	hub.sha = strings.Repeat("0", 64)
	bin := fakeClaude(t)
	claudeEnv(t, hub)
	dir, _ := claudeDir(hub.URL)

	if err := runSync(); !errors.As(err, new(errSilent)) {
		t.Fatalf("sync = %v, want a failure", err)
	}
	if exists(filepath.Join(dir, "plugins", "basics")) {
		t.Error("an archive with the wrong sha256 was extracted")
	}
	if argv := read(t, filepath.Join(bin, "argv")); strings.Contains(argv, "plugin install") {
		t.Errorf("claude was asked to install it; it got:\n%s", argv)
	}
	if len(hub.deliveries) != 2 || hub.deliveries[1]["status"] != "failed" || !strings.Contains(fmt.Sprint(hub.deliveries[1]["error"]), "sha256") {
		t.Errorf("deliveries = %v, want one failed for its sha256", hub.deliveries)
	}
	var m struct{ Plugins []any }
	_ = json.Unmarshal([]byte(read(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"))), &m)
	if len(m.Plugins) != 0 {
		t.Errorf("the marketplace lists a plugin without a folder: %v", m.Plugins)
	}
}

func TestSyncClaudeCodeNameClash(t *testing.T) {
	hub := newFakeHub(t, "tok")
	bin := fakeClaude(t)
	claudeEnv(t, hub)
	dir, _ := claudeDir(hub.URL)
	setClaude(t, bin, `[{"name":"metro-dev","source":"github","repo":"acme/plugins"}]`, `[]`)

	if err := runSync(); err != nil {
		t.Fatalf("sync = %v, want Claude Code skipped, not a failure", err)
	}
	if argv := read(t, filepath.Join(bin, "argv")); argv != "plugin marketplace list --json\n" {
		t.Errorf("claude got more than the list:\n%s", argv)
	}
	if exists(dir) || len(hub.deliveries) != 0 || hub.archiveHits != 0 {
		t.Errorf("a skipped Claude Code still got files (%v), deliveries (%v) or downloads (%d)", exists(dir), hub.deliveries, hub.archiveHits)
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
	if err := saveCreds(hubURL, &creds{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
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

	// No sign-in, then one the hub refuses: each time setup goes to the hub's OAuth (which this fake hub
	// doesn't have, so the sign-in fails there).
	for _, c := range []*creds{nil, {AccessToken: "revoked", ExpiresAt: time.Now().Add(time.Hour)}} {
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
		{name: claudeTarget, where: "metro-dev", delivered: 2, current: 1},
		{name: "pi", where: filepath.Join(home, ".pi", "agent", "skills"), current: 3},
		{name: "codex", where: "/x", delivered: 1, failed: 1},
		{name: claudeTarget, skipped: "the hub has no Claude Code marketplace yet"},
	})
	for _, want := range []string{
		"Claude Code  3 plugins, installed as <plugin>@metro-dev. Next: run /reload-plugins in Claude Code, or restart it.\n",
		"pi           3 plugins in ~/.pi/agent/skills. Nothing new.\n", // no next step
		"Codex        1 plugin in /x. 1 failed: see above, or run `line-lx doctor`. Next: start a new Codex session.\n",
		"Claude Code  nothing installed: the hub has no Claude Code marketplace yet.\n",
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

func TestSyncClaudeCodeLeavesSyncedPluginsAlone(t *testing.T) {
	hub := newFakeHub(t, "tok")
	bin := fakeClaude(t)
	claudeEnv(t, hub)
	dir, _ := claudeDir(hub.URL)
	ours := `[{"name":"metro-dev","source":"directory","path":"` + dir + `"}]`
	synced := `{"id":"basics@synced","version":"1.0.0","scope":"synced","enabled":true}`
	api := &hubClient{hub: hub.URL, token: "tok"}

	// Round 1: line-lx installed it earlier, and now it comes from the claude.ai account too: line-lx's copy goes.
	setClaude(t, bin, ours, `[{"id":"basics@metro-dev","version":"1.0.0","scope":"user"},`+synced+`]`)
	var results []*agentResult
	var err error
	out := captureStdout(t, func() { results, err = syncAll(api) })
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	argv := read(t, filepath.Join(bin, "argv"))
	if !strings.Contains(argv, "plugin uninstall -- basics@metro-dev\n") || strings.Contains(argv, "plugin install") || strings.Contains(argv, "plugin update") {
		t.Errorf("want line-lx's copy uninstalled and nothing installed; claude got:\n%s", argv)
	}
	if !strings.Contains(out, "already in Claude Code through your claude.ai account: removed line-lx's copy") {
		t.Errorf("the output doesn't say why:\n%s", out)
	}
	if len(hub.deliveries) != 0 {
		t.Errorf("a synced plugin was reported as a claude-code delivery: %v", hub.deliveries)
	}
	want := "Claude Code  1 plugin already there through your claude.ai account (<plugin>@synced), not installed again. Next: run /reload-plugins"
	if summary := setupSummary(hub.URL, results); !strings.Contains(summary, want) {
		t.Errorf("summary lacks %q:\n%s", want, summary)
	}

	// Round 2: only the synced one. Nothing to do, nothing reported, and it isn't a failure.
	setClaude(t, bin, ours, `[`+synced+`]`)
	out = captureStdout(t, func() { results, err = syncAll(api) })
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if argv := read(t, filepath.Join(bin, "argv")); strings.Contains(argv, "plugin install") || strings.Contains(argv, "plugin uninstall") {
		t.Errorf("claude got more than the catalog refresh:\n%s", argv)
	}
	if len(hub.deliveries) != 0 || results[0].failed != 0 || results[0].synced != 1 {
		t.Errorf("deliveries %v, result %+v", hub.deliveries, *results[0])
	}
	if summary := setupSummary(hub.URL, results); !strings.Contains(summary, "(<plugin>@synced), not installed again. Nothing new.\n") {
		t.Errorf("summary:\n%s", summary)
	}

	// list and doctor: the synced plugin is fine, not "run line-lx sync"; another synced plugin isn't the hub's.
	installed, _, err := claudeInstalledPlugins(hub.URL, map[string]release{"basics": {name: "basics"}})
	if err != nil || !installed["basics"].synced || hubState(hub.URL, nil, "basics", installed["basics"]) != stateSynced {
		t.Errorf("list sees %+v, %v", installed, err)
	}
	if installed, _, _ := claudeInstalledPlugins(hub.URL, nil); len(installed) != 0 {
		t.Errorf("a synced plugin that isn't the hub's is listed: %v", installed)
	}
}

func TestSyncClaudeCodeFailureWithoutPlugins(t *testing.T) {
	hub := newFakeHub(t, "tok")
	hub.version = "" // the workspace has no plugins
	bin := fakeClaude(t)
	claudeEnv(t, hub)
	setClaude(t, bin, `not json`, `[]`) // claude can't list its marketplaces

	var results []*agentResult
	var err error
	out := captureStdout(t, func() { results, err = syncAll(&hubClient{hub: hub.URL, token: "tok"}) })
	if !errors.As(err, new(errSilent)) {
		t.Fatalf("sync = %v, want a failure", err)
	}
	if !strings.Contains(out, "failed: can't read `claude plugin marketplace list --json`") || !strings.Contains(out, "1 failed.") {
		t.Errorf("the error isn't shown:\n%s", out)
	}
	summary := setupSummary(hub.URL, results)
	if want := "Claude Code  0 plugins. 1 failed: see above"; !strings.Contains(summary, want) || strings.Contains(summary, "@.") {
		t.Errorf("summary lacks %q:\n%s", want, summary)
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
