package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// --- Usage reports: what Claude Code runs of the workspace's plugins, one use at a time. ---
//
// When the workspace turns agent reporting on, the hub's bundle plugin carries a hook that runs
// `line-lx report-usage --agent claude-code --workspace <slug>` in the background after Claude Code runs a
// skill or an MCP tool, and when the person types a skill. line-lx reads the hook's JSON on stdin, keeps
// only what names a part of the workspace's plugins, and sends at most one report to the hub. It never
// slows the agent: it gives up after usageWait, never retries, prints nothing on stdout and always exits 0.
//
// Least data: a name leaves the machine only when it belongs to a plugin Claude Code installed from the
// workspace's marketplace, `metro-<slug>`, and that marketplace is the current hub's. A personal or project
// skill, a plugin from another marketplace, the folder, the session, the prompt and the arguments never do.
// The hub takes the person from the token, and counts the version line-lx reads in Claude Code's cache only
// if it delivered that version to that person.

// usageWait bounds a report from the start of the command: reading stdin, finding the plugin, the
// sign-in's refresh and the request. With the count of a dropped report (dropLockWait at most),
// report-usage ends within 3 seconds.
var usageWait = 2500 * time.Millisecond

// dropLockWait is how long a dropped report waits for another line-lx to finish counting its own.
const dropLockWait = 200 * time.Millisecond

// maxHookInput caps what line-lx reads of the hook's JSON: an MCP tool's answer can be large.
const maxHookInput = 1 << 20

// usageReport is the body of POST /api/usage/reports. The hub refuses any other field.
type usageReport struct {
	Workspace string `json:"workspace"`
	Agent     string `json:"agent"`
	Kind      string `json:"kind"` // activated, mcp_call or failed
	Plugin    string `json:"plugin"`
	Version   string `json:"version"` // the version Claude Code installed, from the plugin's manifest in its cache
	Name      string `json:"name"`
	Trigger   string `json:"trigger"` // person or agent
	Success   bool   `json:"success"`
}

// hookInput is what line-lx reads of Claude Code's hook input; everything else is ignored.
type hookInput struct {
	Event         string                   `json:"hook_event_name"`
	ToolName      string                   `json:"tool_name"`
	ToolInput     json.RawMessage          `json:"tool_input"`
	ToolResponse  json.RawMessage          `json:"tool_response"`
	MCPServer     *struct{ Source string } `json:"mcp_server"`
	ExpansionType string                   `json:"expansion_type"`
	CommandName   string                   `json:"command_name"`
	Cwd           string                   `json:"cwd"` // to look for skills of the same name on this machine; never sent
}

// usage is one use, before line-lx checks that it is the workspace's.
type usage struct {
	kind, trigger string
	success       bool
	skill         string // a skill, as Claude Code names it: `plugin:skill` or `skill`
	tool          string // an MCP tool: mcp__plugin_<plugin>_<server>__<tool>
}

// readUsage maps a Claude Code hook input to a use; false for anything that isn't one.
func readUsage(in hookInput) (usage, bool) {
	switch {
	case in.Event == "UserPromptExpansion" && in.ExpansionType == "slash_command" && in.CommandName != "":
		return usage{kind: "activated", trigger: "person", success: true, skill: in.CommandName}, true
	case in.Event != "PostToolUse" && in.Event != "PostToolUseFailure":
		return usage{}, false
	}
	failed := in.Event == "PostToolUseFailure"
	u := usage{kind: "failed", trigger: "agent"}
	switch {
	case in.ToolName == "Skill":
		var input struct{ Skill string }
		if json.Unmarshal(in.ToolInput, &input) != nil || input.Skill == "" {
			return usage{}, false
		}
		u.skill = input.Skill
		if !failed {
			var res struct{ Success *bool }
			_ = json.Unmarshal(in.ToolResponse, &res) // not an object: a success
			u.kind, u.success = "activated", res.Success == nil || *res.Success
		}
	case strings.HasPrefix(in.ToolName, "mcp__plugin_"):
		if in.MCPServer != nil && in.MCPServer.Source != "" && in.MCPServer.Source != "plugin" {
			return usage{}, false
		}
		u.tool = in.ToolName
		if !failed {
			u.kind, u.success = "mcp_call", true
		}
	default:
		return usage{}, false
	}
	return u, true
}

// --- What Claude Code installed from the workspace's marketplace ---

// pluginCopy is one plugin Claude Code installed: its name, its marketplace and its folder.
type pluginCopy struct{ name, market, dir string }

// installedWorkspace is what Claude Code installed on this machine, and which of it is the workspace's.
type installedWorkspace struct {
	market string       // metro-<slug>
	cache  string       // Claude Code's plugin cache: the workspace's plugins live in <cache>/<market>/<plugin>/
	copies []pluginCopy // every plugin Claude Code installed, from any marketplace
}

// openRegular opens a file only when it is a plain one (a symlink to one is fine): opening a FIFO or a
// device can wait forever.
func openRegular(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a plain file", path)
	}
	return os.Open(path)
}

// readJSONFile decodes a file of at most maxSettings bytes.
func readJSONFile(path string, out any) error {
	f, err := openRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSettings+1))
	if err != nil {
		return err
	}
	if len(data) > maxSettings {
		return fmt.Errorf("%s is over %d MiB", path, maxSettings>>20)
	}
	return json.Unmarshal(data, out)
}

// claudeWorkspace reads Claude Code's records of its marketplaces and plugins. It fails unless Claude Code
// has the workspace's marketplace, `metro-<slug>`, from the hub's URL: plugins of that name from anywhere
// else aren't the workspace's.
func claudeWorkspace(hub, slug string) (*installedWorkspace, error) {
	dir, err := claudeConfigDir()
	if err != nil {
		return nil, err
	}
	plugins := filepath.Join(dir, "plugins")
	w := &installedWorkspace{market: "metro-" + slug, cache: filepath.Join(plugins, "cache")}

	var markets map[string]struct {
		Source struct{ Source, URL string } `json:"source"`
	}
	if err := readJSONFile(filepath.Join(plugins, "known_marketplaces.json"), &markets); err != nil {
		return nil, err
	}
	m, ok := markets[w.market]
	if !ok || m.Source.Source != "url" || strings.TrimRight(m.Source.URL, "/") != marketplaceURL(hub) {
		return nil, fmt.Errorf("Claude Code has no marketplace %s from %s", w.market, hub)
	}

	var installed struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := readJSONFile(filepath.Join(plugins, "installed_plugins.json"), &installed); err != nil {
		return nil, err
	}
	for _, id := range sortedKeys(installed.Plugins) {
		name, market, ok := strings.Cut(id, "@")
		if !ok {
			continue
		}
		for _, entry := range installed.Plugins[id] {
			if entry.InstallPath != "" {
				w.copies = append(w.copies, pluginCopy{name, market, filepath.Clean(entry.InstallPath)})
			}
		}
	}
	return w, nil
}

// ours is true for a plugin of the workspace: installed from its marketplace, in Claude Code's cache for
// that marketplace and plugin. Not the bundle, which has no skills of its own.
func (w *installedWorkspace) ours(p pluginCopy) bool {
	if p.market != w.market || p.name == bundlePlugin || !validName(p.name) {
		return false
	}
	rel, err := filepath.Rel(filepath.Join(w.cache, w.market, p.name), p.dir)
	return err == nil && filepath.IsLocal(rel) && !strings.Contains(rel, string(filepath.Separator))
}

// versionName is a version as a plugin's manifest gives it; versionFolder, a cache folder named like a
// version (Claude Code names some with a hash of the archive instead).
var (
	versionName   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
	versionFolder = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+([-+][0-9A-Za-z.+-]+)?$`)
)

// copyVersion is the version of the plugin installed in dir: its manifest's, else the folder's name when it
// looks like a version.
func copyVersion(dir string) (string, bool) {
	var manifest struct {
		Version *string `json:"version"`
	}
	err := readJSONFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), &manifest)
	switch {
	case err == nil && manifest.Version != nil:
		return *manifest.Version, versionName.MatchString(*manifest.Version)
	case err == nil || errors.Is(err, fs.ErrNotExist):
		return filepath.Base(dir), versionFolder.MatchString(filepath.Base(dir))
	}
	return "", false
}

// plugin returns the folders of the workspace's plugin of that name, and its version. False when there's
// none, and whenever a hook couldn't say which copy ran: a plugin of that name from another marketplace or
// outside the cache is installed too, or Claude Code holds two versions of it (one per scope).
func (w *installedWorkspace) plugin(name string) (dirs []string, version string, ok bool) {
	for _, c := range w.copies {
		if c.name != name {
			continue
		}
		v, known := copyVersion(c.dir)
		if !w.ours(c) || !known || version != "" && v != version {
			return nil, "", false
		}
		dirs, version = append(dirs, c.dir), v
	}
	return dirs, version, len(dirs) > 0
}

// skill resolves a skill as Claude Code names it in a hook, `plugin:skill` or `skill`, to the workspace's
// plugin, its version, and the skill's name in the hub (its folder). A bare `skill` runs a plugin's skill
// only when no other skill or command has the name, so it counts only when the workspace's plugin is the
// one place on this machine that has it (barename.go); cwd is the hook's folder, read here, never sent.
func (w *installedWorkspace) skill(name, cwd string) (plugin, version, skill string, ok bool) {
	name = strings.TrimPrefix(name, "/")
	plugin, s, prefixed := strings.Cut(name, ":")
	if !prefixed {
		return w.bareSkill(name, cwd)
	}
	dirs, version, _ := w.plugin(plugin)
	for _, dir := range dirs {
		if skill, ok := skillIn(dir, plugin, s); ok {
			return plugin, version, skill, true
		}
	}
	return "", "", "", false
}

// skillIn finds a skill of the plugin in dir by its folder or by the name in its SKILL.md (which may carry
// the plugin's prefix), and returns its folder, the skill's name in the hub.
func skillIn(dir, plugin, name string) (string, bool) {
	skills := filepath.Join(dir, "skills")
	if validName(name) && exists(filepath.Join(skills, name, "SKILL.md")) {
		return name, true
	}
	entries, _ := os.ReadDir(skills)
	for _, e := range entries {
		if !validName(e.Name()) {
			continue
		}
		if n := frontmatterName(filepath.Join(skills, e.Name(), "SKILL.md")); n != "" && (n == name || n == plugin+":"+name) {
			return e.Name(), true
		}
	}
	return "", false
}

// frontmatterName is the `name:` of a SKILL.md's frontmatter, or "".
func frontmatterName(path string) string { return frontmatterValue(path, "name") }

// frontmatterValue is a `key:` of a SKILL.md's frontmatter as written on its line, or "".
func frontmatterValue(path, key string) string {
	f, err := openRegular(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	lines := bufio.NewScanner(io.LimitReader(f, 16<<10))
	if !lines.Scan() || strings.TrimSpace(lines.Text()) != "---" {
		return ""
	}
	for lines.Scan() {
		line := lines.Text()
		if strings.TrimSpace(line) == "---" {
			return ""
		}
		if v, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// mcpName is how Claude Code writes a server or tool name in a tool name; mcpUnsafe, what it replaces with
// "_" in a server's name.
var (
	mcpName   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	mcpUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)
)

// mcpTool resolves an MCP tool, mcp__plugin_<plugin>_<server>__<tool>, to the workspace's plugin, its
// version and `<server>__<tool>`. The server must be one the plugin declares.
func (w *installedWorkspace) mcpTool(tool string) (plugin, version, name string, ok bool) {
	rest, _ := strings.CutPrefix(tool, "mcp__plugin_")
	plugin, after, _ := strings.Cut(rest, "_") // plugin names have no "_"
	server, t, _ := strings.Cut(after, "__")
	if !mcpName.MatchString(server) || !mcpName.MatchString(t) {
		return "", "", "", false
	}
	dirs, version, _ := w.plugin(plugin)
	for _, dir := range dirs {
		if declaresServer(dir, server) {
			return plugin, version, server + "__" + t, true
		}
	}
	return "", "", "", false
}

// declaresServer is true when the plugin in dir declares the MCP server, in .mcp.json or in its manifest.
func declaresServer(dir, server string) bool {
	var mcp, manifest map[string]json.RawMessage
	var lists []json.RawMessage
	if readJSONFile(filepath.Join(dir, ".mcp.json"), &mcp) == nil {
		if inner, ok := mcp["mcpServers"]; ok {
			lists = append(lists, inner)
		} else {
			lists = append(lists, rawJSON(mcp))
		}
	}
	if readJSONFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), &manifest) == nil {
		lists = append(lists, manifest["mcpServers"]) // a path to another file isn't followed
	}
	for _, list := range lists {
		var servers map[string]json.RawMessage
		_ = json.Unmarshal(list, &servers)
		for key := range servers {
			if mcpUnsafe.ReplaceAllString(key, "_") == server {
				return true
			}
		}
	}
	return false
}

// --- Sending, and the count of what wasn't sent ---

// usageClient is httpClient's transport, with usageWait as its timeout.
var usageClient = &http.Client{Timeout: usageWait, CheckRedirect: httpClient.CheckRedirect, Transport: httpClient.Transport}

// sendUsage posts one report with the person's sign-in, once: no retry can pile up behind the agent.
func sendUsage(hub string, r usageReport) error {
	token, err := bearer(hub)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(r)
	req, err := http.NewRequest("POST", hub+"/api/usage/reports", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := usageClient.Do(req)
	if err != nil {
		return fmt.Errorf("Can't reach the hub at %s: %v", hub, err)
	}
	res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("The hub answered %d", res.StatusCode)
	}
	return nil
}

// dropped is the count of reports line-lx couldn't send since the last one it did. Nothing else is kept.
type dropped struct {
	Count int       `json:"count"`
	Since time.Time `json:"since"`
}

func droppedPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "line-lx", "usage-dropped.json"), nil
}

// readDropped returns the count; zero when nothing was dropped.
func readDropped() dropped {
	var d dropped
	if path, err := droppedPath(); err == nil {
		_ = readJSONFile(path, &d)
	}
	return d
}

// countDropped adds one to the count, under a lock: the hooks of one session can run at once.
func countDropped(now time.Time) error {
	path, err := droppedPath()
	if err != nil {
		return err
	}
	if err := realDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a plain file (a symlink?), so line-lx won't use it", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|openNoFollow, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil { // a file that was there before keeps its mode otherwise
		return err
	}
	for deadline := time.Now().Add(dropLockWait); ; time.Sleep(10 * time.Millisecond) {
		locked, err := tryLock(f)
		if err != nil {
			return err
		}
		if locked {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("another line-lx holds the count")
		}
	}
	var d dropped
	if data, err := io.ReadAll(io.LimitReader(f, 4<<10)); err == nil {
		_ = json.Unmarshal(data, &d)
	}
	if d.Count == 0 || d.Since.IsZero() {
		d = dropped{Since: now.UTC().Truncate(time.Second)}
	}
	d.Count++
	data, _ := json.Marshal(d)
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(append(data, '\n'), 0)
	return err
}

// clearDropped forgets the count once a report reaches the hub again.
func clearDropped() {
	if path, err := droppedPath(); err == nil {
		_ = os.Remove(path)
	}
}

// droppedText is doctor's line about the count, or "" when nothing was dropped.
func droppedText(d dropped) string {
	if d.Count == 0 {
		return ""
	}
	return fmt.Sprintf("%s dropped since %s: the hub didn't take them. Check the hub and the sign-in above",
		plural(d.Count, "usage report"), d.Since.Local().Format("2006-01-02 15:04"))
}

// --- The command ---

// errNoReport: the input isn't a use of the workspace's plugins, or not surely. Nothing leaves the machine.
var errNoReport = errors.New("nothing to report")

// reportUsage reads the hook's input, finds the workspace's plugin and sends the report, once. It sets
// sending when the report is about to go: past that point, a failure is a dropped report.
func reportUsage(agent, slug string, stdin io.Reader, sending *atomic.Bool) error {
	data, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	var in hookInput
	if err != nil || len(data) > maxHookInput || json.Unmarshal(data, &in) != nil {
		return errNoReport
	}
	u, ok := readUsage(in)
	if !ok {
		return errNoReport
	}
	hub, err := currentHub()
	if err != nil {
		return errNoReport
	}
	w, err := claudeWorkspace(hub, slug)
	if err != nil {
		return errNoReport
	}
	r := usageReport{Workspace: slug, Agent: agent, Kind: u.kind, Trigger: u.trigger, Success: u.success}
	if u.tool != "" {
		r.Plugin, r.Version, r.Name, ok = w.mcpTool(u.tool)
	} else {
		r.Plugin, r.Version, r.Name, ok = w.skill(u.skill, in.Cwd)
	}
	if !ok {
		return errNoReport // not the workspace's, or not surely: it stays on this machine
	}
	sending.Store(true)
	return sendUsage(hub, r)
}

// runReportUsage is what the hub's hook runs. It always returns nil: a report never fails the agent. What
// went wrong goes to stderr, which Claude Code shows only in its debug log, and drops are counted for
// `line-lx doctor`.
func runReportUsage(agent, slug string, stdin io.Reader) error {
	start := time.Now()
	if f, ok := stdin.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintln(os.Stderr, "line-lx report-usage reads a Claude Code hook's input on stdin: the hub's metro-lx plugin runs it.")
			return nil
		}
	}
	if agent != claudeTarget || !validName(slug) || !validName("metro-"+slug) {
		return nil // an agent this line-lx doesn't know yet
	}
	// Everything runs under one deadline: a stdin that never ends, a folder that hangs or a hub that
	// doesn't answer never holds line-lx. What still runs past it ends with the process.
	var sending atomic.Bool
	done := make(chan error, 1)
	go func() { done <- reportUsage(agent, slug, stdin, &sending) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Until(start.Add(usageWait))):
		if !sending.Load() {
			return nil // nothing to send yet: nothing was dropped
		}
		err = fmt.Errorf("The hub didn't answer within %s", usageWait)
	}
	switch {
	case errors.Is(err, errNoReport):
		return nil
	case err == nil:
		clearDropped()
		return nil
	}
	fmt.Fprintf(os.Stderr, "line-lx: usage report dropped: %v\n", err)
	if err := countDropped(time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "line-lx: can't count the dropped report: %v\n", err)
	}
	return nil
}
