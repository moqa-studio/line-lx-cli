package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// --- Claude Code: it gets the hub's plugins from the hub's marketplace by itself. ---
//
// line-lx connects Claude Code once. In its user settings, it declares the hub's marketplace, a url source
// whose headersHelper is `line-lx auth-header`, so Claude Code sends the person's token with each fetch, and
// turns on its auto-update. It enables each plugin of the marketplace, and the hub's bundle plugin,
// metro-lx, which depends on every plugin of the workspace: Claude Code installs, updates and removes them
// by itself (Claude Code 2.1.238 or later), and gets a plugin published later through the bundle.
//
// Claude Code fetches a marketplace its settings declare when it starts, and downloads the enabled plugins
// it lacks, but not the bundle's dependencies. `claude plugin marketplace add` refuses a marketplace whose
// settings entry has a headersHelper (Claude Code 2.1.292), so line-lx installs through `claude` only once
// Claude Code knows the marketplace. Its dependency install brings one missing plugin per run (2.1.292),
// so line-lx installs each missing plugin itself.

// bundlePlugin is the hub's plugin that depends on all the others.
const bundlePlugin = "metro-lx"

// minClaudeVersion is the first Claude Code that runs a marketplace's headersHelper.
const minClaudeVersion = "2.1.238"

// claudeOnPath is true when the claude CLI is on the PATH.
func claudeOnPath() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// claudeConfigDir is Claude Code's configuration folder: $CLAUDE_CONFIG_DIR, else ~/.claude.
func claudeConfigDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// claudeDetected is true when Claude Code is on this machine: its CLI, or its configuration folder (the
// desktop app and the IDE extensions have no CLI on the PATH).
func claudeDetected() bool {
	dir, err := claudeConfigDir()
	return claudeOnPath() || err == nil && isDir(dir)
}

// claudeDir is the folder where line-lx used to write the hub's marketplace for Claude Code, named by a
// hash of the hub URL. line-lx removes it once Claude Code gets the plugins from the hub.
func claudeDir(hub string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "line-lx", "claude-code", hubID(hub)), nil
}

// hubID names a hub's files on this machine: the first 16 hex digits of its URL's sha256.
func hubID(hub string) string {
	sum := sha256.Sum256([]byte(hub))
	return hex.EncodeToString(sum[:8])
}

// durationText is a duration for people: "2 minutes", not "2m0s".
func durationText(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return plural(int(d/time.Minute), "minute")
	}
	return d.String()
}

// marketplaceURL is the hub's Claude Code marketplace.
func marketplaceURL(hub string) string { return hub + "/api/connectors/claude-code/marketplace.json" }

// claudeTimeout bounds one `claude` call: a stalled one (a marketplace update on a dead network, a
// prompt it can't show) must not hold line-lx, nor the agent that runs it.
var claudeTimeout = 2 * time.Minute

// claude runs the claude CLI; on failure the error carries its stderr. Callers put `--` before
// positional arguments, so a name can never be read as a flag. Its stdin is empty: it never prompts.
// A call that takes over claudeTimeout is stopped, and fails.
func claude(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	// Once claude is stopped, a child of it that still holds its output can't keep line-lx waiting either.
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("`claude %s` didn't finish within %s, so line-lx stopped it. Run it yourself to see what it waits for",
			strings.Join(args, " "), durationText(claudeTimeout))
	}
	if err != nil {
		msg := stderr.String()
		if strings.TrimSpace(msg) == "" {
			msg = string(out)
		}
		if strings.TrimSpace(msg) == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("`claude %s`: %s", strings.Join(args, " "), clean(strings.Join(strings.Fields(msg), " ")))
	}
	return out, nil
}

// claudeMarketplaceEntry is one line of `claude plugin marketplace list --json`: a directory marketplace
// has a path, a URL one a url.
type claudeMarketplaceEntry struct{ Name, Source, Path, URL string }

func claudeMarketplaces() ([]claudeMarketplaceEntry, error) {
	out, err := claude("plugin", "marketplace", "list", "--json")
	if err != nil {
		return nil, err
	}
	var list []claudeMarketplaceEntry
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("can't read `claude plugin marketplace list --json`: %v", err)
	}
	return list, nil
}

// ours is true for the hub's marketplace, and for the folder line-lx used to write it to.
func (m claudeMarketplaceEntry) ours(hub, oldDir string) bool {
	return m.Source == "url" && strings.TrimRight(m.URL, "/") == marketplaceURL(hub) ||
		m.Source == "directory" && m.Path != "" && filepath.Clean(m.Path) == oldDir
}

// claudeInstalled maps the plugins Claude Code installed from the marketplace to their versions.
func claudeInstalled(market string) (map[string]string, error) {
	out, err := claude("plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var list []struct{ ID, Version string }
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("can't read `claude plugin list --json`: %v", err)
	}
	installed := map[string]string{}
	for _, p := range list {
		if name, m, ok := strings.Cut(p.ID, "@"); ok && m == market && validName(name) {
			installed[name] = p.Version
		}
	}
	return installed, nil
}

// claudeVersionOK says whether `claude --version` ("2.1.292 (Claude Code)") is at least minClaudeVersion.
// A pre-release or build suffix (-beta.1, +abc) is ignored.
func claudeVersionOK(out string) (version string, ok bool) {
	version, _, _ = strings.Cut(strings.TrimSpace(out), " ")
	parse := func(v string) []int {
		if i := strings.IndexAny(v, "-+"); i >= 0 {
			v = v[:i]
		}
		var nums []int
		for _, part := range strings.Split(v, ".") {
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil
			}
			nums = append(nums, n)
		}
		return nums
	}
	have, want := parse(version), parse(minClaudeVersion)
	return clean(version), len(have) == 3 && slices.Compare(have, want) >= 0
}

// errNoMarketplace and errNoBundle: the hub doesn't serve what Claude Code needs (yet). Not a failure:
// sync skips the target.
var (
	errNoMarketplace = errors.New("the hub has no Claude Code marketplace yet")
	errNoBundle      = errors.New("the hub's Claude Code marketplace has no " + bundlePlugin + " plugin yet: the hub needs an update")
)

// errMarketplaceClash: Claude Code already has another marketplace under the hub's marketplace name, for
// example the Claude connector's GitHub repository added by hand. Adding ours would replace it, so line-lx
// leaves Claude Code alone. Not a failure: sync skips the target.
type errMarketplaceClash struct{ name, from string }

func (e errMarketplaceClash) Error() string {
	return fmt.Sprintf("Claude Code already has a marketplace named %s, from %s, so line-lx leaves it alone. "+
		"To get the plugins from line-lx instead, run `claude plugin marketplace remove %s`, then `line-lx sync`", e.name, clean(e.from), e.name)
}

// claudeSkipped is true for the reasons sync skips Claude Code instead of failing.
func claudeSkipped(err error) bool {
	var clash errMarketplaceClash
	return errors.Is(err, errNoMarketplace) || errors.Is(err, errNoBundle) || errors.As(err, &clash)
}

// hubCatalog is what line-lx needs from the hub's marketplace: its name, its owner (the workspace), and
// its plugins but the bundle.
type hubCatalog struct {
	name, owner string
	plugins     []string
}

// hubMarketplace fetches the hub's Claude Code marketplace (errNoMarketplace on 404, errNoBundle without
// the bundle plugin).
func hubMarketplace(api *hubClient) (*hubCatalog, error) {
	var doc struct {
		Name    string
		Owner   struct{ Name string }
		Plugins []struct{ Name string }
	}
	if err := api.do("GET", "/api/connectors/claude-code/marketplace.json", nil, &doc); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusNotFound {
			return nil, errNoMarketplace
		}
		return nil, err
	}
	if !validName(doc.Name) {
		return nil, errors.New("the hub's marketplace has no valid name")
	}
	cat := &hubCatalog{name: doc.Name, owner: clean(doc.Owner.Name)}
	if cat.owner == "" {
		cat.owner = "your workspace"
	}
	bundle := false
	for _, p := range doc.Plugins {
		switch {
		case p.Name == bundlePlugin:
			bundle = true
		case validName(p.Name) && !slices.Contains(cat.plugins, p.Name):
			cat.plugins = append(cat.plugins, p.Name)
		}
	}
	if !bundle {
		return nil, errNoBundle
	}
	return cat, nil
}

// plainPath is a path sh runs as written.
var plainPath = regexp.MustCompile(`^[A-Za-z0-9_/._+@-]+$`)

// helperCommand is the headersHelper Claude Code runs through sh: this binary's absolute path, then
// auth-header. When the line-lx on the PATH is this same binary, its path is used instead: Homebrew's
// /opt/homebrew/bin/line-lx stays the same across upgrades.
func helperCommand() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if onPath, err := exec.LookPath("line-lx"); err == nil && filepath.IsAbs(onPath) {
		a, errA := os.Stat(onPath)
		b, errB := os.Stat(path)
		if errA == nil && errB == nil && os.SameFile(a, b) {
			path = onPath
		}
	}
	return helperFor(path)
}

// helperFor is the headersHelper that runs the line-lx at path. A path that isn't plain is quoted. Claude
// Code takes a command of at most 500 characters of printable ASCII, without a run of four or more spaces.
func helperFor(path string) (string, error) {
	printable := !strings.ContainsFunc(path, func(r rune) bool { return r < ' ' || r > '~' || r == '\'' })
	switch {
	case plainPath.MatchString(path):
	case printable && !strings.Contains(path, "    "): // Claude Code refuses four spaces in a row
		path = "'" + path + "'"
	default:
		return "", fmt.Errorf("Claude Code can't run line-lx from %q: move it to a folder whose path is plain ASCII, without quotes", clean(path))
	}
	if cmd := path + " auth-header"; len(cmd) <= 500 {
		return cmd, nil
	}
	return "", fmt.Errorf("Claude Code can't run line-lx from %s: the path is too long", clean(path))
}

// --- Claude Code's user settings ---

func claudeSettingsPath() (string, error) {
	dir, err := claudeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// maxSettings caps what line-lx reads of a settings file.
const maxSettings = 1 << 20

// readClaudeSettings reads Claude Code's user settings.
func readClaudeSettings() (s map[string]json.RawMessage, path string, mode os.FileMode, err error) {
	path, err = claudeSettingsPath()
	if err != nil {
		return nil, "", 0, err
	}
	return readSettingsFile(path)
}

// readSettingsFile reads a settings file, its mode and its real path (settings.json is often a symlink to
// a dotfiles repository). A missing file is empty settings. A file that isn't a JSON object, is over
// maxSettings, or is a symlink to nothing is refused, never replaced.
func readSettingsFile(path string) (s map[string]json.RawMessage, real string, mode os.FileMode, err error) {
	real, err = filepath.EvalSymlinks(path)
	if err != nil {
		if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, "", 0, fmt.Errorf("%s is a symlink to nothing, so line-lx won't change it", path)
		}
		real = path
	}
	s, mode = map[string]json.RawMessage{}, 0o644
	f, err := os.Open(real)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, real, mode, nil
	case err != nil:
		return nil, "", 0, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSettings+1))
	switch {
	case err != nil:
		return nil, "", 0, err
	case len(data) > maxSettings:
		return nil, "", 0, fmt.Errorf("%s is over %d MiB, so line-lx won't change it", real, maxSettings>>20)
	}
	if err := json.Unmarshal(data, &s); err != nil || s == nil {
		return nil, "", 0, fmt.Errorf("%s isn't valid JSON, so line-lx won't change it. Fix it, then run `line-lx sync`", real)
	}
	if info, err := f.Stat(); err == nil {
		mode = info.Mode().Perm()
	}
	return s, real, mode, nil
}

// writeClaudeSettings writes settings through a temp file and a rename in the same folder, with mode.
func writeClaudeSettings(s map[string]json.RawMessage, path string, mode os.FileMode) error {
	var data bytes.Buffer
	if err := json.Indent(&data, rawJSON(s), "", "  "); err != nil {
		return err
	}
	data.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings.json.line-lx-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, err = tmp.Write(data.Bytes())
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// settingsObject reads one of the settings' objects; an absent key is an empty one.
func settingsObject(s map[string]json.RawMessage, key, path string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if raw, ok := s[key]; ok && (json.Unmarshal(raw, &out) != nil || out == nil) {
		return nil, fmt.Errorf("%s in %s isn't an object, so line-lx won't change it", key, path)
	}
	return out, nil
}

// settingsMarket is an extraKnownMarketplaces entry.
type settingsMarket struct {
	Source struct {
		Source        string `json:"source"`
		URL           string `json:"url,omitempty"`
		Path          string `json:"path,omitempty"`
		HeadersHelper string `json:"headersHelper,omitempty"`
	} `json:"source"`
	AutoUpdate bool `json:"autoUpdate,omitempty"`
}

func (m settingsMarket) entry() claudeMarketplaceEntry {
	return claudeMarketplaceEntry{Source: m.Source.Source, Path: m.Source.Path, URL: m.Source.URL}
}

// connectSettings declares the hub's marketplace in settings, under its name, and enables its plugins.
// Entries for this hub under another name (the workspace's former slug) or for line-lx's former folder go,
// with the plugins enabled from them. It refuses another marketplace under the hub's name.
func connectSettings(s map[string]json.RawMessage, path string, cat *hubCatalog, hub, oldDir, helper string) error {
	markets, err := settingsObject(s, "extraKnownMarketplaces", path)
	if err != nil {
		return err
	}
	enabled, err := settingsObject(s, "enabledPlugins", path)
	if err != nil {
		return err
	}
	for name, raw := range markets {
		var m settingsMarket
		_ = json.Unmarshal(raw, &m)
		ours := m.entry().ours(hub, oldDir)
		switch {
		case name == cat.name && !ours:
			from := m.Source.URL + m.Source.Path
			if from == "" {
				from = m.Source.Source
			}
			return errMarketplaceClash{cat.name, from}
		case name != cat.name && ours:
			delete(markets, name)
			for id := range enabled {
				if strings.HasSuffix(id, "@"+name) {
					delete(enabled, id)
				}
			}
		}
	}
	var m settingsMarket
	m.Source.Source, m.Source.URL, m.Source.HeadersHelper, m.AutoUpdate = "url", marketplaceURL(hub), helper, true
	markets[cat.name] = rawJSON(m)

	// Each plugin of the catalog, enabled: Claude Code downloads an enabled plugin it lacks when it starts,
	// which it doesn't do for the bundle's dependencies. A plugin the person turned off stays off; one the
	// hub no longer has goes. The bundle brings the plugins published later, when Claude Code auto-updates.
	enabled[bundlePlugin+"@"+cat.name] = json.RawMessage("true")
	for id := range enabled {
		if plugin, ok := strings.CutSuffix(id, "@"+cat.name); ok && plugin != bundlePlugin && !slices.Contains(cat.plugins, plugin) {
			delete(enabled, id)
		}
	}
	for _, plugin := range cat.plugins {
		if _, set := enabled[plugin+"@"+cat.name]; !set {
			enabled[plugin+"@"+cat.name] = json.RawMessage("true")
		}
	}
	s["extraKnownMarketplaces"], s["enabledPlugins"] = rawJSON(markets), rawJSON(enabled)
	return nil
}

// rawJSON encodes v without HTML escaping, so the other settings keep their characters as written: a
// hook's `&&` stays `&&`, not `&&`.
func rawJSON(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSpace(b.Bytes())
}

// claudeManagedDir is where Claude Code reads its managed settings files on this OS
// (https://code.claude.com/docs/en/managed-settings). line-lx can't see MDM profiles or server-managed settings.
var claudeManagedDir = map[string]string{"darwin": "/Library/Application Support/ClaudeCode", "linux": "/etc/claude-code"}[runtime.GOOS]

// claudeSettingsFiles are the files whose settings can connect Claude Code to the hub, the one that wins
// first: the managed drop-ins (the last in alphabetical order wins), managed-settings.json, then the
// person's settings.
func claudeSettingsFiles() []string {
	var files []string
	if claudeManagedDir != "" {
		dropIns, _ := filepath.Glob(filepath.Join(claudeManagedDir, "managed-settings.d", "*.json"))
		slices.Reverse(dropIns)
		for _, f := range dropIns {
			if !strings.HasPrefix(filepath.Base(f), ".") { // Claude Code ignores hidden files
				files = append(files, f)
			}
		}
		files = append(files, filepath.Join(claudeManagedDir, "managed-settings.json"))
	}
	if user, err := claudeSettingsPath(); err == nil {
		files = append(files, user)
	}
	return files
}

// claudeConnection reads how Claude Code's settings, managed or the person's, connect it to the hub: the
// marketplace's name ("" when they don't), the file that declares it, and what's wrong with the
// connection, if anything. A managed file line-lx can't read is passed over.
func claudeConnection(hub string) (name, where, problem string, err error) {
	user, _ := claudeSettingsPath()
	var market settingsMarket
	enabled := map[string]json.RawMessage{} // for each plugin, the file that wins
	for _, path := range claudeSettingsFiles() {
		s, real, _, err := readSettingsFile(path)
		var markets, en map[string]json.RawMessage
		if err == nil {
			markets, err = settingsObject(s, "extraKnownMarketplaces", real)
		}
		if err == nil {
			en, err = settingsObject(s, "enabledPlugins", real)
		}
		switch {
		case err != nil && path == user:
			return "", "", "", err
		case err != nil:
			continue
		}
		for id, v := range en {
			if _, ok := enabled[id]; !ok {
				enabled[id] = v
			}
		}
		for _, key := range sortedKeys(markets) {
			var m settingsMarket
			if name == "" && json.Unmarshal(markets[key], &m) == nil && m.Source.Source == "url" &&
				strings.TrimRight(m.Source.URL, "/") == marketplaceURL(hub) {
				name, where, market = key, path, m
			}
		}
	}
	if name == "" {
		return "", "", "", nil
	}
	bin, isHelper := strings.CutSuffix(market.Source.HeadersHelper, " auth-header")
	if unquoted, ok := strings.CutPrefix(bin, "'"); ok {
		bin = strings.TrimSuffix(unquoted, "'")
	}
	_, runErr := exec.LookPath(bin)
	switch {
	case !isHelper:
		problem = "its marketplace doesn't sign in with `line-lx auth-header`"
	case runErr != nil:
		problem = fmt.Sprintf("its marketplace runs %s, which isn't an executable file", clean(bin))
	case !market.AutoUpdate:
		problem = "its marketplace doesn't auto-update"
	case string(enabled[bundlePlugin+"@"+name]) != "true":
		problem = bundlePlugin + "@" + name + " isn't enabled"
	}
	return name, where, problem, nil
}

// claudeBlocked says why sync would skip Claude Code, or can't connect it, from the hub's marketplace and
// Claude Code's settings, without changing anything. Nil when sync can go ahead.
func claudeBlocked(api *hubClient) error {
	cat, err := hubMarketplace(api)
	if err != nil {
		return err
	}
	oldDir, err := claudeDir(api.hub)
	if err != nil {
		return err
	}
	s, path, _, err := readClaudeSettings()
	if err != nil {
		return err
	}
	// A dry run on a copy: the helper doesn't matter for these checks, so it's left empty.
	return connectSettings(s, path, cat, api.hub, oldDir, "")
}

// claudeConnect connects Claude Code to the hub, and says what it did in one sentence. Running it again
// changes nothing, but installs the plugins Claude Code still lacks.
func claudeConnect(api *hubClient) (string, error) {
	cat, err := hubMarketplace(api)
	if err != nil {
		return "", err
	}
	oldDir, err := claudeDir(api.hub)
	if err != nil {
		return "", err
	}
	helper, err := helperCommand()
	if err != nil {
		return "", err
	}
	// Refuse before changing anything: settings line-lx can't change, or another marketplace under the name.
	s, path, _, err := readClaudeSettings()
	if err == nil {
		err = connectSettings(s, path, cat, api.hub, oldDir, helper)
	}
	if err != nil {
		return "", err
	}

	known, onPath := false, claudeOnPath()
	var stale []string
	if onPath {
		list, err := claudeMarketplaces()
		if err != nil {
			return "", err
		}
		for _, m := range list {
			switch {
			case m.Name == cat.name && m.Source == "url" && strings.TrimRight(m.URL, "/") == marketplaceURL(api.hub):
				known = true
			case m.ours(api.hub, oldDir): // line-lx's former folder, or the workspace's former name
				stale = append(stale, m.Name)
			case m.Name == cat.name:
				return "", errMarketplaceClash{cat.name, m.Path + m.URL}
			}
		}
		// Removing a marketplace uninstalls its plugins: Claude Code installs them again from the hub.
		for _, name := range stale {
			if !validName(name) {
				return "", fmt.Errorf("Claude Code has the hub's marketplace under an invalid name %q", clean(name))
			}
			if _, err := claude("plugin", "marketplace", "remove", "--", name); err != nil {
				return "", err
			}
		}
	}

	// Read again: removing a marketplace changes the settings.
	s, path, mode, err := readClaudeSettings()
	if err != nil {
		return "", err
	}
	before, _ := json.Marshal(s)
	var old settingsMarket
	if markets, err := settingsObject(s, "extraKnownMarketplaces", path); err == nil {
		_ = json.Unmarshal(markets[cat.name], &old)
	}
	if err := connectSettings(s, path, cat, api.hub, oldDir, helper); err != nil {
		return "", err
	}
	after, _ := json.Marshal(s)
	changed := !bytes.Equal(before, after)
	if changed {
		if err := writeClaudeSettings(s, path, mode); err != nil {
			return "", err
		}
	}
	// Claude Code fetches the marketplace again from a source its settings changed, when it starts; until
	// then, its CLI doesn't list it. So line-lx installs now only from a marketplace whose source it kept.
	now := known && old.Source.Source == "url" && old.Source.URL == marketplaceURL(api.hub) && old.Source.HeadersHelper == helper
	if now {
		// Each enabled plugin Claude Code hasn't installed, then the bundle. `claude plugin install` refreshes
		// the marketplace first.
		installed, err := claudeInstalled(cat.name)
		if err != nil {
			return "", err
		}
		enabled, _ := settingsObject(s, "enabledPlugins", path)
		for _, plugin := range append(slices.Clone(cat.plugins), bundlePlugin) {
			_, has := installed[plugin]
			if has || string(enabled[plugin+"@"+cat.name]) != "true" {
				continue
			}
			if _, err := claude("plugin", "install", "--", plugin+"@"+cat.name); err != nil {
				return "", err
			}
		}
	}
	// line-lx's former folder, once Claude Code no longer reads plugins from it.
	if onPath {
		if err := os.RemoveAll(oldDir); err != nil {
			return "", err
		}
		_ = os.Remove(filepath.Dir(oldDir)) // only when no other hub's folder is left
	}

	gets := fmt.Sprintf("Claude Code gets %s's plugins from the hub", cat.owner)
	switch {
	case now && !changed:
		return "already connected: " + gets + " by itself.", nil
	case now:
		return "connected: " + gets + " by itself. Next: run /reload-plugins in Claude Code, or restart it.", nil
	}
	// Claude Code doesn't know the marketplace (yet): it fetches it when it starts.
	msg := "connected: " + gets + " from its next start."
	if len(stale) > 0 || old.Source.Source == "directory" {
		msg += " The plugins line-lx installed before come back when Claude Code starts again."
	}
	return msg, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
