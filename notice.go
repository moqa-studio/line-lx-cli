package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// --- The session notice: what Claude Code shows the person when a session starts. ---
//
// The hub's bundle plugin, metro-lx, runs `line-lx notice --agent claude-code --workspace <slug>` when a Claude
// Code session starts or resumes, as a synchronous SessionStart hook. line-lx checks this machine's sign-in,
// Claude Code's version and the workspace's plugins Claude Code installed, against what the hub delivers
// (GET /api/me/installed, the read `line-lx status` makes), and prints one JSON object, {"systemMessage": "…"},
// which Claude Code shows the person and never gives Claude, or nothing at all. All is well: nothing.
//
// - One line when something is wrong, the most important first, with the fix.
// - A line per plugin updated since the last session, with its changelog's first line, once: a small state
//   file in line-lx's config folder (notice.json) remembers the versions it last saw.
//
// It never slows a session: it answers within noticeWait, with what it knows by then. A sign-in refresh is the
// one thing it waits for past that (refreshGrace at most): dropped half-way, the hub would have rotated the
// refresh token and this machine would keep the revoked one. It sends nothing new: the request is status's, with
// the person's sign-in. It always exits 0, and prints nothing on stdout but that one object: plain stdout would
// reach Claude's context.

// noticeWait bounds the notice from the start of the command.
var noticeWait = 1500 * time.Millisecond

// refreshGrace is how long past noticeWait the notice waits for a sign-in refresh under way.
var refreshGrace = 2500 * time.Millisecond

// behindGrace: a plugin is behind only once its version has been out this long. Claude Code updates in a
// person's first interactive session after a publish, up to 10 minutes after the first message.
const behindGrace = 24 * time.Hour

const (
	maxChangelog     = 120 // a changelog line, in characters
	maxNoticeUpdates = 3   // updated plugins named one by one
	maxNoticeNames   = 4   // plugins named in a problem
	maxNoticePlugins = 500 // plugins the state remembers
	maxNoticeState   = 64 << 10
	noticeLockWait   = 100 * time.Millisecond
)

// notice is one problem: what's wrong, and the fix.
type notice struct{ text, fix string }

// noticeFacts is what the notice knows about this machine and the hub, checked: names are valid and text is clean.
type noticeFacts struct {
	hub, slug string
	signIn    error  // why the sign-in doesn't work (errNotSignedIn, errSignInRefused); nil when it does, or line-lx can't tell
	elsewhere string // the host the workspace's marketplace in Claude Code comes from, when it isn't line-lx's hub
	local     claudeLocal
	off       map[string]bool // plugins the person turned off in Claude Code's settings
	claude    string          // Claude Code's version, when line-lx could tell
	hv        hubView
	now       time.Time
}

// changelogLine is the first line of a changelog with text in it, as a person reads it: the first that isn't a
// Markdown heading, else the first heading, without Markdown's leading marks, cleaned for the terminal, on one
// line, and cut to maxChangelog characters.
func changelogLine(s *string) string {
	if s == nil {
		return ""
	}
	heading := ""
	for _, raw := range strings.Split(*s, "\n") {
		text := bounded(raw, 4*maxChangelog)
		line := strings.TrimSpace(strings.TrimLeft(text, "#>*-+ "))
		switch {
		case line == "":
		case !strings.HasPrefix(text, "#"):
			return cutLine(line)
		case heading == "":
			heading = line
		}
	}
	return cutLine(heading)
}

// cutLine keeps at most maxChangelog characters of a line, the last one an ellipsis when it's cut.
func cutLine(line string) string {
	if utf8.RuneCountInString(line) <= maxChangelog {
		return line
	}
	return strings.TrimSpace(string([]rune(line)[:maxChangelog-1])) + "…"
}

// lineLxCall finds the line-lx command a hook runs: the hub's hooks wrap the session notice in shell.
var lineLxCall = regexp.MustCompile(`(?:^|[\s(])line-lx ([a-z][a-z-]{0,30})(?:\s|$)`)

// hookRuns says what a hook runs, for status: the line-lx command, or the whole command.
func hookRuns(command string) string {
	if m := lineLxCall.FindStringSubmatch(command); m != nil {
		return "runs line-lx " + m[1]
	}
	return "runs " + command
}

// names lists plugin names for a sentence: at most maxNoticeNames, then how many more.
func names(list []string) string {
	if len(list) <= maxNoticeNames {
		return strings.Join(list, ", ")
	}
	return strings.Join(list[:maxNoticeNames-1], ", ") + fmt.Sprintf(" and %d more", len(list)-maxNoticeNames+1)
}

// claudeLag compares the workspace's plugins Claude Code installed here with the hub's latest: those it lacks
// and those it has in an older version, among the versions out for behindGrace. A plugin the person turned off
// isn't counted.
func claudeLag(f noticeFacts) (missing, behind []string) {
	if !hubSays(f) {
		return nil, nil
	}
	for _, name := range sortedKeys(f.hv.agents[claudeTarget].plugins) {
		p := f.hv.agents[claudeTarget].plugins[name]
		if name == bundlePlugin || p.latest == nil || f.off[name] || p.published == nil || f.now.Sub(*p.published) < behindGrace {
			continue
		}
		switch l, ok := f.local.plugins[name]; {
		case !ok:
			missing = append(missing, name)
		case l.version != *p.latest:
			behind = append(behind, name)
		}
	}
	slices.SortFunc(missing, func(a, b string) int { return f.hv.order[a] - f.hv.order[b] })
	slices.SortFunc(behind, func(a, b string) int { return f.hv.order[a] - f.hv.order[b] })
	return missing, behind
}

// hubSays is true when the hub answered for the workspace whose plugins Claude Code has: another workspace's
// plugins and changelogs say nothing about these.
func hubSays(f noticeFacts) bool {
	return f.hv.status == hubOK && f.hv.workspace != nil && f.hv.workspace.Slug == f.slug
}

// signInNotice says why Claude Code gets nothing from the hub when the sign-in doesn't work.
func signInNotice(err error) notice {
	what := "was refused by the hub"
	var none errNotSignedIn
	if errors.As(err, &none) {
		what = map[string]string{
			"not signed in":           "is missing",
			"unreadable":              "can't be read",
			"from an earlier line-lx": "is from an earlier line-lx",
			"expired":                 "expired",
		}[none.why]
	}
	if what == "" {
		what = "doesn't work"
	}
	return notice{"Claude Code gets no plugin updates: your line-lx sign-in " + what + ".", "Run `line-lx login`."}
}

// noticeProblems is what's wrong, the most important first: the sign-in, the connection, the workspace, Claude
// Code's version, then the plugins.
func noticeProblems(f noticeFacts) []notice {
	var out []notice
	if f.signIn != nil {
		out = append(out, signInNotice(f.signIn))
	}
	if f.elsewhere != "" {
		out = append(out, notice{fmt.Sprintf("Claude Code takes your workspace's plugins from %s, but line-lx is signed in to %s, so they don't update.", f.elsewhere, hostOf(f.hub)), "Run `line-lx doctor`."})
	}
	if f.local.problem != "" {
		out = append(out, notice{"Claude Code's connection to the hub: " + strings.TrimSuffix(bounded(f.local.problem, 300), ".") + ".", "Run `line-lx doctor`."})
	}
	if f.hv.status == hubOK && f.hv.workspace != nil && f.hv.workspace.Slug != f.slug {
		out = append(out, notice{fmt.Sprintf("Claude Code has the plugins of %s, but your line-lx sign-in now acts in %s.", f.slug, f.hv.workspace.Slug), "Run `line-lx sync`, or switch back in the web app."})
	}
	if f.claude != "" {
		if version, ok := claudeVersionOK(f.claude); !ok {
			out = append(out, notice{fmt.Sprintf("Claude Code %s is older than %s, so new plugins won't arrive.", bounded(version, 40), minClaudeVersion), "Run `claude update`."})
		}
	}
	missing, behind := claudeLag(f)
	switch len(missing) {
	case 0:
	case 1:
		out = append(out, notice{missing[0] + " isn't installed yet.", "Run `line-lx sync`."})
	default:
		out = append(out, notice{fmt.Sprintf("%d plugins aren't installed yet: %s.", len(missing), names(missing)), "Run `line-lx sync`."})
	}
	if len(behind) > 0 {
		what := behind[0] + " is"
		if len(behind) > 1 {
			what = fmt.Sprintf("%d plugins are", len(behind))
		}
		text := fmt.Sprintf("%s behind the hub for over a day: %s.", what, names(behind))
		if len(behind) == 1 {
			text = fmt.Sprintf("%s behind the hub for over a day.", what)
		}
		out = append(out, notice{text, "Send a message: Claude Code updates within 10 minutes. Then run `/reload-plugins`."})
	}
	return out
}

// noticeUpdates names the plugins whose version changed here since the last session (seen), with their
// changelog line when the hub gave it for that version, and says what to remember now.
func noticeUpdates(seen map[string]string, f noticeFacts) (lines []string, now map[string]string) {
	now = map[string]string{}
	for _, name := range sortedKeys(f.local.plugins) {
		if name != bundlePlugin && f.local.plugins[name].version != "" && len(now) < maxNoticePlugins {
			now[name] = f.local.plugins[name].version
		}
	}
	if seen == nil { // the first time, or another hub or workspace: nothing is new yet
		return nil, now
	}
	var updated []string
	for _, name := range sortedKeys(now) {
		if seen[name] != now[name] {
			updated = append(updated, name)
		}
	}
	slices.SortStableFunc(updated, func(a, b string) int { return f.hv.order[a] - f.hv.order[b] })
	for i, name := range updated {
		if i == maxNoticeUpdates && len(updated) > maxNoticeUpdates+1 {
			lines = append(lines, fmt.Sprintf("Metro-LX updated %d more plugins: run `line-lx status`.", len(updated)-i))
			break
		}
		verb, version := "updated "+name+" to", "v"+now[name]
		if _, had := seen[name]; !had {
			verb = "added " + name
		}
		line := "Metro-LX " + verb + " " + version
		if p, ok := f.hv.agents[claudeTarget].plugins[name]; ok && hubSays(f) && p.latest != nil && *p.latest == now[name] && p.changelog != "" {
			line += ": " + p.changelog
		}
		if !strings.HasSuffix(line, ".") && !strings.HasSuffix(line, "!") && !strings.HasSuffix(line, "?") && !strings.HasSuffix(line, "…") {
			line += "."
		}
		lines = append(lines, line)
	}
	return lines, now
}

// noticeMessage is what Claude Code shows: the first problem with its fix, then the updates. Empty when there's nothing.
func noticeMessage(problems []notice, updates []string) string {
	var lines []string
	if len(problems) > 0 {
		line := "Metro-LX: " + problems[0].text + " " + problems[0].fix
		if more := len(problems) - 1; more > 0 {
			line += fmt.Sprintf(" (%s: run `line-lx doctor`.)", plural(more, "more problem"))
		}
		lines = append(lines, line)
	}
	return strings.Join(append(lines, updates...), "\n")
}

// --- What the notice remembers ---

// noticeState is notice.json: the plugin versions the notice last saw for one hub and workspace, and Claude Code's
// version for its binary as it was, so `claude --version` runs only when Claude Code changes. Nothing else.
type noticeState struct {
	Hub       string            `json:"hub"`
	Workspace string            `json:"workspace"`
	Plugins   map[string]string `json:"plugins"`
	Claude    *claudeSeen       `json:"claude,omitempty"`
}

// claudeSeen is the version a Claude Code binary told, with what identifies the binary.
type claudeSeen struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	Version string    `json:"version"`
}

func noticePath(name string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "line-lx", name), nil
}

// readNoticeState reads notice.json; a missing, oversized or unreadable one is no state.
func readNoticeState() noticeState {
	var s noticeState
	path, err := noticePath("notice.json")
	if err != nil {
		return s
	}
	f, err := openRegular(path)
	if err != nil {
		return s
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxNoticeState+1))
	if err != nil || len(data) > maxNoticeState || json.Unmarshal(data, &s) != nil {
		return noticeState{}
	}
	for name, v := range s.Plugins {
		if !validName(name) || !versionName.MatchString(v) {
			delete(s.Plugins, name)
		}
	}
	return s
}

// updateNoticeState changes notice.json under a lock (notice.lock: two sessions can start at once), and writes it
// whole, through a temporary file and a rename, so a reader never sees half of it. change says whether to write.
// It fails when the lock isn't free within noticeLockWait: the other line-lx tells the updates.
func updateNoticeState(change func(*noticeState) bool) error {
	lockPath, err := noticePath("notice.lock")
	if err != nil {
		return err
	}
	dir := filepath.Dir(lockPath)
	if err := realDir(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a plain file (a symlink?), so line-lx won't use it", lockPath)
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|openNoFollow, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for deadline := time.Now().Add(noticeLockWait); ; time.Sleep(10 * time.Millisecond) {
		locked, err := tryLock(lock)
		if err != nil {
			return err
		}
		if locked {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("another line-lx holds the notice's state")
		}
	}
	s := readNoticeState()
	if !change(&s) {
		return nil
	}
	data, _ := json.Marshal(s)
	tmp, err := os.CreateTemp(dir, ".notice-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone after the rename; left only by a failure
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	path, _ := noticePath("notice.json")
	return os.Rename(tmp.Name(), path)
}

// --- What the notice reads ---

// claudeVersionNow is the version of the `claude` on the PATH, from the state while its binary is unchanged,
// else from `claude --version`, which must end by deadline. "" when there's no claude or it doesn't say.
func claudeVersionNow(seen *claudeSeen, deadline time.Time) (string, *claudeSeen) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", nil
	}
	path, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil
	}
	now := claudeSeen{Path: path, Size: info.Size(), ModTime: info.ModTime().UTC()}
	if seen != nil && seen.Path == now.Path && seen.Size == now.Size && seen.ModTime.Equal(now.ModTime) && seen.Version != "" {
		return seen.Version, seen
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	// "2.1.320 (Claude Code)": the version is the first word.
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if version = bounded(version, 40); version == "" {
		return "", nil
	}
	now.Version = version
	return version, &now
}

// marketplaceElsewhere is the host the workspace's marketplace in Claude Code's records comes from, when it isn't
// the hub's: Claude Code then asks line-lx for a sign-in to a hub it isn't signed in to.
func marketplaceElsewhere(hub, slug string) string {
	dir, err := claudeConfigDir()
	if err != nil {
		return ""
	}
	var markets map[string]struct {
		Source struct{ Source, URL string } `json:"source"`
	}
	if readJSONFile(filepath.Join(dir, "plugins", "known_marketplaces.json"), &markets) != nil {
		return ""
	}
	m, ok := markets["metro-"+slug]
	if !ok || m.Source.Source != "url" || strings.TrimRight(m.Source.URL, "/") == marketplaceURL(hub) {
		return ""
	}
	u, err := url.Parse(m.Source.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	return bounded(u.Host, 100)
}

// turnedOff is the workspace's plugins the person turned off in Claude Code's user settings.
func turnedOff(market string) map[string]bool {
	off := map[string]bool{}
	s, path, _, err := readClaudeSettings()
	if err != nil || market == "" {
		return off
	}
	enabled, err := settingsObject(s, "enabledPlugins", path)
	if err != nil {
		return off
	}
	for id, v := range enabled {
		if name, m, ok := strings.Cut(id, "@"); ok && m == market && string(v) == "false" {
			off[name] = true
		}
	}
	return off
}

// noticeHub is what the hub said: why the sign-in doesn't work, or what it delivers.
type noticeHub struct {
	signIn error
	hv     hubView
}

// askNoticeHub asks the hub what it delivers to the person, as status does. A sign-in that doesn't work is said;
// a hub that doesn't answer isn't: offline is no problem to show.
func askNoticeHub(hub string, gate *atomic.Int32) noticeHub {
	token, err := watchedBearer(hub, gate)
	var none errNotSignedIn
	switch {
	case errors.As(err, &none):
		return noticeHub{signIn: err}
	case err != nil:
		return noticeHub{}
	}
	hv := askInstalled(&hubClient{hub: hub, token: token})
	if hv.status == hubRefused {
		return noticeHub{signIn: errSignInRefused{hub}}
	}
	return noticeHub{hv: hv}
}

// --- The command ---

// runNotice is what the hub's session hook runs. It prints the systemMessage object or nothing, and returns nil
// whatever happens: a notice never fails a session.
func runNotice(agent, slug string, stdout io.Writer) error {
	start := time.Now()
	if agent != claudeTarget || !validName(slug) || !validName("metro-"+slug) {
		return nil // an agent this line-lx doesn't know yet
	}
	hub, err := currentHub()
	if err != nil {
		return nil
	}
	deadline := start.Add(noticeWait)
	state := readNoticeState()

	// The hub and Claude Code's version at once, while line-lx reads this machine.
	var gate atomic.Int32
	fromHub := make(chan noticeHub, 1)
	go func() { fromHub <- askNoticeHub(hub, &gate) }()
	type version struct {
		v    string
		seen *claudeSeen
	}
	fromClaude := make(chan version, 1)
	go func() {
		v, seen := claudeVersionNow(state.Claude, deadline)
		fromClaude <- version{v, seen}
	}()

	f := noticeFacts{hub: hub, slug: slug, now: start, local: readClaudeLocal(hub, slug), elsewhere: marketplaceElsewhere(hub, slug)}
	f.off = turnedOff("metro-" + slug)

	timeout := time.After(time.Until(deadline))
	var h noticeHub
	var cv version
	gotHub, gotClaude := false, false
	for !gotHub || !gotClaude {
		select {
		case h = <-fromHub:
			gotHub = true
		case cv = <-fromClaude:
			gotClaude = true
		case <-timeout:
			// No refresh starts now, and one under way is waited for: the hub rotates the refresh token as it
			// answers. What came in meanwhile counts; the rest is unknown.
			closeGate(&gate, time.Now().Add(refreshGrace))
			select {
			case h = <-fromHub:
			default:
			}
			select {
			case cv = <-fromClaude:
			default:
			}
			gotHub, gotClaude = true, true
		}
	}
	f.signIn, f.hv, f.claude = h.signIn, h.hv, cv.v

	// The plugins updated since the last session, once: remembered under a lock before they're told, and told
	// only once remembered.
	var updates, told []string
	err = updateNoticeState(func(s *noticeState) bool {
		seen := s.Plugins
		if s.Hub != hub || s.Workspace != slug {
			seen = nil
		}
		lines, now := noticeUpdates(seen, f)
		// Without the hub's answer there's no changelog to tell: what changed is told next time.
		if seen != nil && !hubSays(f) {
			lines, now = nil, seen
		}
		if cv.seen != nil {
			s.Claude = cv.seen
		}
		s.Hub, s.Workspace, s.Plugins = hub, slug, now
		told = lines
		return true
	})
	if err == nil {
		updates = told
	}

	message := noticeMessage(noticeProblems(f), updates)
	if message == "" {
		return nil
	}
	data, _ := json.Marshal(map[string]string{"systemMessage": message})
	_, _ = stdout.Write(append(data, '\n'))
	return nil
}
