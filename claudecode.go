package main

import (
	"archive/zip"
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
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// --- Claude Code: line-lx writes the hub's marketplace to a local folder; the `claude` CLI installs from it. ---
//
// The hub's marketplace and its plugin archives need the person's token, which Claude Code can't send. So
// line-lx fetches them with its own, checks each archive against the marketplace's sha256, and writes a
// directory marketplace Claude Code reads: <config dir>/line-lx/claude-code/<hub id>/, holding
// .claude-plugin/marketplace.json and plugins/<name>/. Claude Code reads a directory marketplace's plugins
// from that folder, and `claude plugin update` records a new version written there (Claude Code 2.1.292).

const claudeRestart = "Claude Code loads plugins on launch: run /reload-plugins in it, or restart it"

// Limits on one plugin archive. The hub takes at most 2 MB per version, so these leave plenty of room.
const (
	maxArchive        = 16 << 20 // as downloaded
	maxArchiveEntry   = 16 << 20 // one file, uncompressed
	maxArchiveTotal   = 64 << 20 // all files, uncompressed
	maxArchiveEntries = 5000
)

func claudeDetected() (string, bool) {
	path, err := exec.LookPath("claude")
	return path, err == nil
}

// claudeDir is the hub's marketplace folder, named by a hash of the hub URL: one folder per hub, and a
// plain folder name whatever the URL.
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

// marketplaceURL is the hub's marketplace. A marketplace added to Claude Code by this URL can't
// authenticate (Claude Code sends no token), so sync removes it.
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

func (m claudeMarketplaceEntry) isFolder(dir string) bool {
	return m.Source == "directory" && m.Path != "" && filepath.Clean(m.Path) == dir
}

// claudeMarketplace finds the hub's marketplace among Claude Code's, by its folder. Empty when not added.
func claudeMarketplace(hub string) (string, error) {
	dir, err := claudeDir(hub)
	if err != nil {
		return "", err
	}
	list, err := claudeMarketplaces()
	if err != nil {
		return "", err
	}
	for _, m := range list {
		if m.isFolder(dir) {
			if !validName(m.Name) {
				return "", fmt.Errorf("the hub's marketplace is added under an invalid name %q", clean(m.Name))
			}
			return m.Name, nil
		}
	}
	return "", nil
}

// claudeInstalled maps the plugins Claude Code installed from the marketplace to their versions, and the
// ones it synced from the person's claude.ai account to theirs. Claude Code lists a synced plugin with
// the scope "synced" and the id <name>@synced (2.1.292): with the Claude connector, the workspace's
// plugins, under the same names.
func claudeInstalled(market string) (installed, synced map[string]string, err error) {
	out, err := claude("plugin", "list", "--json")
	if err != nil {
		return nil, nil, err
	}
	var list []struct{ ID, Version, Scope string }
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, nil, fmt.Errorf("can't read `claude plugin list --json`: %v", err)
	}
	installed, synced = map[string]string{}, map[string]string{}
	for _, p := range list {
		name, m, ok := strings.Cut(p.ID, "@")
		switch {
		case !ok || !validName(name):
		case p.Scope == "synced" || p.Scope == "" && m == "synced":
			synced[name] = p.Version
		case m == market:
			installed[name] = p.Version
		}
	}
	return installed, synced, nil
}

// errNoMarketplace: the hub doesn't serve a Claude Code marketplace (yet). Not a failure: sync skips the target.
var errNoMarketplace = errors.New("the hub has no Claude Code marketplace yet")

// errMarketplaceClash: Claude Code already has another marketplace under the hub's marketplace name, for
// example the Claude connector's GitHub repository added by hand. Adding ours would replace it, so line-lx
// leaves Claude Code alone. Not a failure: sync skips the target.
type errMarketplaceClash struct{ name, from string }

func (e errMarketplaceClash) Error() string {
	return fmt.Sprintf("Claude Code already has a marketplace named %s, from %s, so line-lx leaves it alone. "+
		"To get the plugins from line-lx instead, run `claude plugin marketplace remove %s`, then `line-lx sync`", e.name, clean(e.from), e.name)
}

// hubCatalog is the hub's marketplace.json, kept whole: the copy line-lx writes changes only each plugin's source.
type hubCatalog struct {
	name    string
	doc     map[string]json.RawMessage
	plugins []catalogPlugin
}

type catalogPlugin struct {
	name, url, sha256 string
	entry             map[string]json.RawMessage
	err               error // the entry is unusable: the plugin fails, the others go on
}

var sha256Rule = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hubMarketplace fetches the hub's Claude Code marketplace (errNoMarketplace on 404). Entries without a
// valid name are dropped; an entry line-lx can't fetch carries its error.
func hubMarketplace(api *hubClient) (*hubCatalog, error) {
	var doc map[string]json.RawMessage
	if err := api.do("GET", "/api/connectors/claude-code/marketplace.json", nil, &doc); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusNotFound {
			return nil, errNoMarketplace
		}
		return nil, err
	}
	cat := &hubCatalog{doc: doc}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(doc["name"], &cat.name) != nil || !validName(cat.name) || json.Unmarshal(doc["plugins"], &entries) != nil {
		return nil, fmt.Errorf("the hub's marketplace has no valid name or plugin list")
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		p := catalogPlugin{entry: entry}
		if json.Unmarshal(entry["name"], &p.name) != nil || !validName(p.name) || seen[p.name] {
			continue
		}
		seen[p.name] = true
		var src struct{ Source, URL, Sha256 string }
		if err := json.Unmarshal(entry["source"], &src); err != nil || src.Source != "archive" || !sha256Rule.MatchString(src.Sha256) {
			p.err = errors.New("the hub's marketplace has no archive with a sha256 for it")
		}
		p.url, p.sha256 = src.URL, src.Sha256
		cat.plugins = append(cat.plugins, p)
	}
	return cat, nil
}

// claudeMarket is the hub's marketplace as line-lx wrote it on this machine.
type claudeMarket struct {
	name    string          // the name Claude Code knows it by
	dir     string          // its folder
	listed  map[string]bool // the plugins in its marketplace.json
	failed  map[string]error
	entries map[string]map[string]json.RawMessage // each plugin's entry in the hub's marketplace
}

// claudePrepare writes the hub's marketplace to its folder and makes sure Claude Code knows it, with its
// latest catalog. A plugin whose new version can't be written is in failed; its folder keeps the
// previous version, if any.
func claudePrepare(api *hubClient) (*claudeMarket, error) {
	cat, err := hubMarketplace(api)
	if err != nil {
		return nil, err
	}
	dir, err := claudeDir(api.hub)
	if err != nil {
		return nil, err
	}
	ours, stale, err := claudeFind(api.hub, dir, cat.name)
	if err != nil {
		return nil, err
	}

	listed, failed, err := writeMarketplace(api, cat, dir)
	if err != nil {
		return nil, err
	}
	for _, name := range stale {
		if !validName(name) {
			continue
		}
		if _, err := claude("plugin", "marketplace", "remove", "--", name); err != nil {
			return nil, err
		}
	}
	// The workspace's slug changed: its marketplace has a new name, so it's added again under that name.
	if ours != nil && ours.Name != cat.name {
		if validName(ours.Name) {
			if _, err := claude("plugin", "marketplace", "remove", "--", ours.Name); err != nil {
				return nil, err
			}
		}
		ours = nil
	}
	if ours == nil {
		_, err = claude("plugin", "marketplace", "add", "--", dir)
	} else {
		_, err = claude("plugin", "marketplace", "update", "--", cat.name)
	}
	if err != nil {
		return nil, err
	}
	entries := map[string]map[string]json.RawMessage{}
	for _, p := range cat.plugins {
		entries[p.name] = p.entry
	}
	return &claudeMarket{name: cat.name, dir: dir, listed: listed, failed: failed, entries: entries}, nil
}

// claudeFind looks through Claude Code's marketplaces for the hub's folder (ours), marketplaces added by
// this hub's URL, which can't authenticate (stale), and another marketplace under the hub's name (a clash).
func claudeFind(hub, dir, name string) (ours *claudeMarketplaceEntry, stale []string, err error) {
	list, err := claudeMarketplaces()
	if err != nil {
		return nil, nil, err
	}
	for _, m := range list {
		switch {
		case m.isFolder(dir):
			ours = &m
		case m.Source == "url" && strings.TrimRight(m.URL, "/") == marketplaceURL(hub):
			stale = append(stale, m.Name)
		case m.Name == name:
			from := m.Path + m.URL
			if from == "" {
				from = m.Source
			}
			return nil, nil, errMarketplaceClash{name, from}
		}
	}
	return ours, stale, nil
}

// claudeBlocked says why sync would skip Claude Code: the hub has no marketplace, or another marketplace
// has its name. Nil when sync can go ahead.
func claudeBlocked(api *hubClient) error {
	cat, err := hubMarketplace(api)
	if err != nil {
		return err
	}
	dir, err := claudeDir(api.hub)
	if err != nil {
		return err
	}
	_, _, err = claudeFind(api.hub, dir, cat.name)
	return err
}

// marketStamp records the archive each plugin folder was extracted from, so an unchanged plugin isn't
// downloaded again.
type marketStamp struct {
	Hub     string            `json:"hub"`
	Plugins map[string]string `json:"plugins"` // name → sha256
}

// writeMarketplace writes the hub's catalog to dir. Each plugin is extracted to a staging folder, then
// renamed into place, so Claude Code never reads half a plugin; marketplace.json goes last, through a
// rename too, and lists only the plugins that have a folder.
func writeMarketplace(api *hubClient, cat *hubCatalog, dir string) (listed map[string]bool, failed map[string]error, err error) {
	base, plugins := filepath.Dir(dir), filepath.Join(dir, "plugins")
	staging := filepath.Join(base, ".staging")
	for _, p := range []string{base, dir, plugins, filepath.Join(dir, ".claude-plugin"), staging} {
		if err := realDir(p); err != nil {
			return nil, nil, err
		}
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.RemoveAll(staging); err != nil { // leftovers of an interrupted sync
		return nil, nil, err
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(staging)

	stampPath := filepath.Join(dir, ".line-lx.json")
	var old marketStamp
	if data, err := os.ReadFile(stampPath); err == nil {
		_ = json.Unmarshal(data, &old)
	}
	st := marketStamp{Hub: api.hub, Plugins: map[string]string{}}
	listed, failed = map[string]bool{}, map[string]error{}
	var entries []map[string]json.RawMessage
	for _, p := range cat.plugins {
		dest := filepath.Join(plugins, p.name)
		sha := old.Plugins[p.name]
		switch {
		case p.err != nil:
			failed[p.name] = p.err
		case sha == p.sha256 && old.Hub == api.hub && isDir(dest):
		default:
			if err := fetchPlugin(api, p, filepath.Join(staging, p.name), dest); err != nil {
				failed[p.name] = err
			} else {
				sha = p.sha256
			}
		}
		if !isDir(dest) {
			continue
		}
		st.Plugins[p.name] = sha
		listed[p.name] = true
		source, _ := json.Marshal("./plugins/" + p.name)
		entry := map[string]json.RawMessage{}
		for k, v := range p.entry {
			entry[k] = v
		}
		entry["source"] = source
		entries = append(entries, entry)
	}
	if err := writeJSON(stampPath, st); err != nil {
		return nil, nil, err
	}
	doc := map[string]json.RawMessage{}
	for k, v := range cat.doc {
		doc[k] = v
	}
	if entries == nil {
		entries = []map[string]json.RawMessage{}
	}
	doc["plugins"], _ = json.Marshal(entries)
	return listed, failed, writeJSON(filepath.Join(dir, ".claude-plugin", "marketplace.json"), doc)
}

// fetchPlugin downloads a plugin's archive, checks it against the marketplace's sha256, extracts it to
// tmp and moves it to dest.
func fetchPlugin(api *hubClient, p catalogPlugin, tmp, dest string) error {
	data, err := api.download(p.url, maxArchive)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != p.sha256 {
		return fmt.Errorf("the archive of %s doesn't match the sha256 in the hub's marketplace", p.name)
	}
	if err := extractPlugin(data, tmp, p.name); err != nil {
		return fmt.Errorf("the archive of %s: %v", p.name, err)
	}
	return replaceDir(tmp, dest)
}

// extractPlugin unzips a plugin archive into dest, a folder it creates. It takes only plain files and
// folders at local paths, each once, within the size limits, and needs .claude-plugin/plugin.json to
// name the plugin it is for. Every write goes through an os.Root on dest, so nothing lands outside it.
func extractPlugin(data []byte, dest, plugin string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("not a zip archive: %v", err)
	}
	if len(zr.File) > maxArchiveEntries {
		return fmt.Errorf("over %d entries", maxArchiveEntries)
	}
	if err := os.Mkdir(dest, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()

	seen := map[string]bool{}
	var total int64
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		folder := name != f.Name
		if !localPath(name) {
			return fmt.Errorf("unsafe path %q", clean(f.Name))
		}
		// Case-insensitive: on macOS two such names are one file.
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("%q is in it twice", clean(name))
		}
		seen[key] = true
		mode := f.Mode()
		switch {
		case folder && mode.IsDir():
			if err := root.MkdirAll(filepath.FromSlash(name), 0o755); err != nil {
				return err
			}
			continue
		case folder || !mode.IsRegular():
			return fmt.Errorf("%q isn't a plain file or folder", clean(f.Name))
		}
		if parent := path.Dir(name); parent != "." {
			if err := root.MkdirAll(filepath.FromSlash(parent), 0o755); err != nil {
				return err
			}
		}
		n, err := extractFile(root, f, filepath.FromSlash(name))
		if err != nil {
			return err
		}
		if total += n; total > maxArchiveTotal {
			return fmt.Errorf("over %d MiB uncompressed", maxArchiveTotal>>20)
		}
	}

	manifest, err := root.ReadFile(filepath.Join(".claude-plugin", "plugin.json"))
	if err != nil {
		return errors.New("no .claude-plugin/plugin.json")
	}
	var m struct{ Name string }
	if err := json.Unmarshal(manifest, &m); err != nil || m.Name != plugin {
		return fmt.Errorf(".claude-plugin/plugin.json isn't for %s", plugin)
	}
	return nil
}

// extractFile writes one archive entry, which must not exist yet: O_EXCL also refuses two names the
// filesystem takes as one (case, Unicode normalization). It returns the size written.
func extractFile(root *os.Root, f *zip.File, name string) (int64, error) {
	in, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, maxArchiveEntry+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxArchiveEntry {
		err = fmt.Errorf("%q is over %d MiB", clean(f.Name), maxArchiveEntry>>20)
	}
	return n, err
}

// prune removes the plugin folders not in keep: plugins the hub no longer has, once uninstalled.
func (m *claudeMarket) prune(keep map[string]bool) error {
	entries, err := os.ReadDir(filepath.Join(m.dir, "plugins"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			if err := os.RemoveAll(filepath.Join(m.dir, "plugins", e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// runs says what a plugin installed in Claude Code runs on this machine without asking: its hooks'
// events and its MCP servers. Empty when it has neither.
func (m *claudeMarket) runs(plugin string) string {
	return pluginRuns(filepath.Join(m.dir, "plugins", plugin), m.entries[plugin])
}

// pluginRuns names the events of a plugin's hooks/hooks.json and the servers of its .mcp.json. Hooks or MCP
// servers set in its plugin.json or its marketplace entry are named by kind only, and no path is followed.
func pluginRuns(dir string, entry map[string]json.RawMessage) string {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ""
	}
	defer root.Close()
	var manifest map[string]json.RawMessage
	if data, ok := readSmall(root, ".claude-plugin/plugin.json"); ok {
		_ = json.Unmarshal(data, &manifest)
	}
	var parts []string
	for _, k := range []struct{ label, file, key string }{
		{"hooks", "hooks/hooks.json", "hooks"},
		{"MCP servers", ".mcp.json", "mcpServers"},
	} {
		names, found := configNames(root, k.file, k.key)
		if declares(manifest[k.key]) || declares(entry[k.key]) {
			found = true
			if len(names) > 0 {
				names = append(names, "others")
			}
		}
		switch {
		case !found:
		case len(names) == 0:
			parts = append(parts, k.label)
		default:
			parts = append(parts, k.label+" ("+listNames(names)+")")
		}
	}
	return strings.Join(parts, " · ")
}

// configNames reads the keys of a hooks or MCP file, maybe wrapped in key as in {"hooks": {"PreToolUse": …}}.
// found is false when the file is missing, null or an empty object; any other file that isn't a JSON object
// is found, without names, so a hook or server line-lx can't read is never left out.
func configNames(root *os.Root, file, key string) (names []string, found bool) {
	data, ok := readSmall(root, file)
	if !ok {
		return nil, false
	}
	var config, inner map[string]json.RawMessage
	if json.Unmarshal(data, &config) != nil {
		return nil, true
	}
	if json.Unmarshal(config[key], &inner) == nil && inner != nil {
		config = inner
	}
	return sortedKeys(config), len(config) > 0
}

// declares says whether a field of plugin.json or of the marketplace entry is set: not missing, null or empty.
func declares(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	return s != "" && s != "null" && s != "{}" && s != "[]" && s != `""`
}

// listNames joins names for the terminal: cleaned, at most 10, then how many more.
func listNames(names []string) string {
	var out []string
	for _, n := range names[:min(len(names), 10)] {
		out = append(out, clean(n))
	}
	if len(names) > 10 {
		out = append(out, fmt.Sprintf("+%d more", len(names)-10))
	}
	return strings.Join(out, ", ")
}

// maxConfig caps what line-lx reads of a plugin's plugin.json, hooks or MCP file.
const maxConfig = 1 << 20

// readSmall reads at most maxConfig bytes of a file of the plugin. ok is false when it can't be opened,
// as through a symlink out of the plugin's folder.
func readSmall(root *os.Root, file string) ([]byte, bool) {
	f, err := root.Open(filepath.FromSlash(file))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfig))
	return data, err == nil
}

func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}
