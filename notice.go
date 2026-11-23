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
	"strconv"
	"strings"
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
// - A line per plugin updated since the last session, with its changelog's first line, once: a state file in
//   line-lx's config folder (notice.json) remembers what it told, per hub and workspace.
//
// It never slows a session: it answers within noticeWait, with what it knows by then. It never refreshes the
// sign-in itself: a refresh rotates the refresh token at the hub, and a process that may stop at any moment must
// never hold one half-way. When the access token expired, it starts `notice --refresh` as a process of its own,
// which refreshes it, asks the hub, and records the answer for the next session; this one uses the last answer
// recorded. It sends nothing new: the requests are auth-header's and status's, with the person's sign-in. It
// always exits 0, and prints nothing on stdout but that one object: plain stdout would reach Claude's context.

// noticeWait bounds the notice from the start of the command.
var noticeWait = 1500 * time.Millisecond

// behindGrace: a plugin is behind only once its version has been out this long. Claude Code updates in a
// person's first interactive session after a publish, up to 10 minutes after the first message.
const behindGrace = 24 * time.Hour

// answerMaxAge: an older recorded answer of the hub says nothing about what's behind.
const answerMaxAge = 3 * 24 * time.Hour

// refreshWait bounds `notice --refresh`, which nobody waits for: past the sign-in's own timeouts, it stops.
var refreshWait = 2 * time.Minute

const (
	maxChangelog     = 120 // a changelog line, in characters
	maxNoticeUpdates = 3   // updated plugins named one by one
	maxNoticeNames   = 4   // plugins named in a problem
	maxNoticePlugins = 200 // plugins remembered per workspace
	maxNoticeSpaces  = 16  // hubs and workspaces remembered, the least recently used dropped first
	maxNoticeState   = 1 << 20
	noticeLockWait   = 100 * time.Millisecond
	refreshLockWait  = 2 * time.Second
)

// notice is one problem: what's wrong, and the fix.
type notice struct{ text, fix string }

// hubFacts is what the hub said for one workspace, now or at the last answer recorded: the workspace the sign-in
// acts in, and the latest version of each of its Claude Code plugins but the bundle.
type hubFacts struct {
	actsIn  string
	plugins map[string]latestRecord
}

// noticeFacts is what the notice knows about this machine and the hub, checked: names are valid and text is clean.
type noticeFacts struct {
	hub, slug string
	signIn    error  // why the sign-in doesn't work (errNotSignedIn, errSignInRefused); nil when it does, or line-lx can't tell
	elsewhere string // the host the workspace's marketplace in Claude Code comes from, when it isn't line-lx's hub
	local     claudeLocal
	off       map[string]bool // plugins the person turned off in Claude Code's settings
	claude    string          // Claude Code's version, when line-lx could tell
	answer    *hubFacts       // nil: the hub didn't say, and nothing recent is recorded
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

// compareVersions orders two plugin versions by their numbers (1.10.0 after 1.9.0): negative, 0 or positive. A
// pre-release comes before its release. 0 when either isn't numbers.
func compareVersions(a, b string) int {
	parse := func(v string) ([]int, bool) {
		core, pre, _ := strings.Cut(strings.SplitN(v, "+", 2)[0], "-")
		var out []int
		for _, part := range strings.Split(core, ".") {
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, false
			}
			out = append(out, n)
		}
		return out, pre != ""
	}
	x, xPre := parse(a)
	y, yPre := parse(b)
	if x == nil || y == nil {
		return 0
	}
	if c := slices.Compare(x, y); c != 0 || xPre == yPre {
		return c
	}
	if xPre {
		return -1
	}
	return 1
}

// answerFor is the hub's answer for the workspace whose plugins Claude Code has: another workspace's plugins and
// changelogs say nothing about these.
func answerFor(f noticeFacts) *hubFacts {
	if f.answer == nil || f.answer.actsIn != f.slug {
		return nil
	}
	return f.answer
}

// claudeLag compares the workspace's plugins Claude Code installed here with the hub's latest: those it lacks
// and those it has in an older version, among the versions out for behindGrace. A plugin the person turned off
// isn't counted.
func claudeLag(f noticeFacts) (missing, behind []string) {
	a := answerFor(f)
	if a == nil {
		return nil, nil
	}
	for _, name := range sortedKeys(a.plugins) {
		p := a.plugins[name]
		if f.off[name] || p.Published == nil || f.now.Sub(*p.Published) < behindGrace {
			continue
		}
		switch l, ok := f.local.plugins[name]; {
		case !ok:
			missing = append(missing, name)
		case l.version != p.Version:
			behind = append(behind, name)
		}
	}
	byOrder := func(x, y string) int { return a.plugins[x].Order - a.plugins[y].Order }
	slices.SortStableFunc(missing, byOrder)
	slices.SortStableFunc(behind, byOrder)
	return missing, behind
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
	if f.answer != nil && f.answer.actsIn != "" && f.answer.actsIn != f.slug {
		out = append(out, notice{fmt.Sprintf("Claude Code has the plugins of %s, but your line-lx sign-in now acts in %s.", f.slug, f.answer.actsIn), "Run `line-lx sync`, or switch back in the web app."})
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
		text := behind[0] + " is behind the hub for over a day."
		if len(behind) > 1 {
			text = fmt.Sprintf("%d plugins are behind the hub for over a day: %s.", len(behind), names(behind))
		}
		out = append(out, notice{text, "Send a message: Claude Code updates within 10 minutes. Then run `/reload-plugins`."})
	}
	return out
}

// updateLine says one plugin's new version, and quotes its changelog as its publisher's words, never as Metro-LX's.
func updateLine(name, version string, added bool, changelog string) string {
	line := "Metro-LX · " + name + " updated to v" + version
	if added {
		line = "Metro-LX · " + name + " v" + version + " added"
	}
	if changelog != "" {
		line += ` · its changelog: "` + strings.ReplaceAll(changelog, `"`, "'") + `"`
	}
	return line
}

// noticeUpdates names the plugins whose version changed here since what was told (told), and says what to
// remember now. A version is told once the hub's answer knows it, with its changelog line; until then it waits. A
// version older than the one told is remembered quietly: a downgrade isn't an update. With no told yet (the first
// session for this hub and workspace), everything here is remembered and nothing is told.
func noticeUpdates(told map[string]string, f noticeFacts) (lines []string, now map[string]string) {
	now = map[string]string{}
	here := map[string]string{}
	for _, name := range sortedKeys(f.local.plugins) {
		if name != bundlePlugin && f.local.plugins[name].version != "" && len(here) < maxNoticePlugins {
			here[name] = f.local.plugins[name].version
		}
	}
	if told == nil {
		return nil, here
	}
	a := answerFor(f)
	var updated []string
	for _, name := range sortedKeys(here) {
		was, had := told[name]
		switch {
		case had && was == here[name]:
			now[name] = was
		case had && compareVersions(here[name], was) < 0:
			now[name] = here[name]
		case a != nil && a.plugins[name].Version == here[name]:
			now[name] = here[name]
			updated = append(updated, name)
		case had:
			now[name] = was // told when the hub knows this version
		}
	}
	if a != nil {
		slices.SortStableFunc(updated, func(x, y string) int { return a.plugins[x].Order - a.plugins[y].Order })
	}
	for i, name := range updated {
		if i == maxNoticeUpdates && len(updated) > maxNoticeUpdates+1 {
			lines = append(lines, fmt.Sprintf("Metro-LX · %d more plugins updated: run `line-lx status`.", len(updated)-i))
			break
		}
		_, had := told[name]
		lines = append(lines, updateLine(name, here[name], !had, a.plugins[name].Changelog))
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

// noticeState is notice.json: per hub and workspace, the plugin versions told and the hub's last answer; per
// hub, the workspace the sign-in acts in and a refresh the hub refused; Claude Code's version for its binary as
// it was, so `claude --version` runs only when Claude Code changes. Nothing else, and no secret.
type noticeState struct {
	Spaces []spaceRecord `json:"spaces,omitempty"` // the most recently used first
	Hubs   []hubRecord   `json:"hubs,omitempty"`
	Claude *claudeSeen   `json:"claude,omitempty"`
}

type spaceRecord struct {
	Hub        string                  `json:"hub"`
	Workspace  string                  `json:"workspace"`
	Told       map[string]string       `json:"told"`             // each plugin's version told, or seen at the first session
	Latest     map[string]latestRecord `json:"latest,omitempty"` // the hub's last answer
	AnsweredAt time.Time               `json:"answeredAt,omitzero"`
	UsedAt     time.Time               `json:"usedAt"`
}

// latestRecord is a plugin's latest version in the hub's answer, with when it came out, its changelog's first line
// and its place in the hub's order.
type latestRecord struct {
	Version   string     `json:"version"`
	Published *time.Time `json:"published,omitempty"`
	Changelog string     `json:"changelog,omitempty"`
	Order     int        `json:"order"`
}

type hubRecord struct {
	Hub      string    `json:"hub"`
	ActsIn   string    `json:"actsIn,omitempty"`  // the workspace the sign-in acts in, at the last answer
	Expired  bool      `json:"expired,omitempty"` // the hub refused the last refresh of the sign-in that expires at TokenAt
	TokenAt  time.Time `json:"tokenAt,omitzero"`
	AnswerAt time.Time `json:"answerAt,omitzero"`
}

// claudeSeen is the version a Claude Code binary told, with what identifies the binary.
type claudeSeen struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	Version string    `json:"version"`
}

func (s *noticeState) space(hub, slug string) *spaceRecord {
	for i := range s.Spaces {
		if s.Spaces[i].Hub == hub && s.Spaces[i].Workspace == slug {
			return &s.Spaces[i]
		}
	}
	return nil
}

func (s *noticeState) hub(hub string) *hubRecord {
	for i := range s.Hubs {
		if s.Hubs[i].Hub == hub {
			return &s.Hubs[i]
		}
	}
	return nil
}

// touchSpace returns the record of a hub and workspace, made and moved first, the oldest dropped past maxNoticeSpaces.
func (s *noticeState) touchSpace(hub, slug string, now time.Time) *spaceRecord {
	rec := spaceRecord{Hub: hub, Workspace: slug}
	if old := s.space(hub, slug); old != nil {
		rec = *old
	}
	rec.UsedAt = now
	rest := slices.DeleteFunc(s.Spaces, func(r spaceRecord) bool { return r.Hub == hub && r.Workspace == slug })
	s.Spaces = append([]spaceRecord{rec}, rest[:min(len(rest), maxNoticeSpaces-1)]...)
	return &s.Spaces[0]
}

func (s *noticeState) touchHub(hub string) *hubRecord {
	rec := hubRecord{Hub: hub}
	if old := s.hub(hub); old != nil {
		rec = *old
	}
	rest := slices.DeleteFunc(s.Hubs, func(r hubRecord) bool { return r.Hub == hub })
	s.Hubs = append([]hubRecord{rec}, rest[:min(len(rest), maxNoticeSpaces-1)]...)
	return &s.Hubs[0]
}

// recordAnswer keeps the hub's answer for the next sessions: the workspace the sign-in acts in, and that
// workspace's Claude Code plugins, bounded.
func (s *noticeState) recordAnswer(hub string, hv hubView, now time.Time) {
	if hv.status != hubOK || hv.workspace == nil {
		return
	}
	h := s.touchHub(hub)
	h.ActsIn, h.AnswerAt, h.Expired = hv.workspace.Slug, now, false
	sp := s.touchSpace(hub, hv.workspace.Slug, now)
	sp.Latest, sp.AnsweredAt = factsOf(hv).plugins, now
}

// factsOf is the hub's answer as the notice reads it.
func factsOf(hv hubView) *hubFacts {
	if hv.status != hubOK || hv.workspace == nil {
		return nil
	}
	f := &hubFacts{actsIn: hv.workspace.Slug, plugins: map[string]latestRecord{}}
	for _, name := range sortedKeys(hv.agents[claudeTarget].plugins) {
		p := hv.agents[claudeTarget].plugins[name]
		if name == bundlePlugin || p.latest == nil || len(f.plugins) >= maxNoticePlugins {
			continue
		}
		f.plugins[name] = latestRecord{Version: *p.latest, Published: p.published, Changelog: p.changelog, Order: hv.order[name]}
	}
	return f
}

// recorded is the last answer recorded for a hub and workspace, while it is recent enough to count.
func (s noticeState) recorded(hub, slug string, now time.Time) *hubFacts {
	h, sp := s.hub(hub), s.space(hub, slug)
	if h == nil || now.Sub(h.AnswerAt) > answerMaxAge {
		return nil
	}
	f := &hubFacts{actsIn: h.ActsIn}
	if sp != nil && now.Sub(sp.AnsweredAt) <= answerMaxAge {
		f.plugins = sp.Latest
	}
	return f
}

func noticePath(name string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "line-lx", name), nil
}

// readNoticeState reads notice.json; a missing, oversized or unreadable one is no state. What doesn't check out
// (names, versions, text) is dropped.
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
	s.Spaces = slices.DeleteFunc(s.Spaces, func(r spaceRecord) bool { return !validName(r.Workspace) || r.Told == nil })
	s.Spaces = s.Spaces[:min(len(s.Spaces), maxNoticeSpaces)]
	s.Hubs = s.Hubs[:min(len(s.Hubs), maxNoticeSpaces)]
	for i := range s.Spaces {
		for name, v := range s.Spaces[i].Told {
			if !validName(name) || !versionName.MatchString(v) {
				delete(s.Spaces[i].Told, name)
			}
		}
		for name, p := range s.Spaces[i].Latest {
			if !validName(name) || !versionName.MatchString(p.Version) {
				delete(s.Spaces[i].Latest, name)
				continue
			}
			p.Changelog = changelogLine(&p.Changelog)
			s.Spaces[i].Latest[name] = p
		}
	}
	for i := range s.Hubs {
		if !validName(s.Hubs[i].ActsIn) {
			s.Hubs[i].ActsIn = ""
		}
	}
	return s
}

// updateNoticeState changes notice.json under a lock (notice.lock: sessions, and the refresh, can run at once),
// and writes it whole, through a temporary file and a rename, so a reader never sees half of it. change says
// whether to write. It fails when the lock isn't free within wait.
func updateNoticeState(wait time.Duration, change func(*noticeState) bool) error {
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
	for deadline := time.Now().Add(wait); ; time.Sleep(10 * time.Millisecond) {
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

// --- The sign-in's refresh, in a process of its own ---

// startRefresh starts `line-lx notice --refresh` in its own session, detached from the hook: Claude Code may stop
// the hook and its process group at any time, and the refresh must end and be saved whatever happens to them. Its
// standard streams are the null device, so the hook's output closes without it. A variable for the tests.
var startRefresh = func(slug string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "notice", "--refresh", "--agent", claudeTarget, "--workspace", slug)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// runNoticeRefresh refreshes the sign-in as every command does, asks the hub what it delivers, and records the
// answer for the next session, or that the hub refused the refresh. One at a time (refresh.lock); nobody waits for
// it, so it keeps the normal timeouts, within refreshWait.
func runNoticeRefresh() error {
	hub, err := currentHub()
	if err != nil {
		return nil
	}
	lockPath, err := noticePath("refresh.lock")
	if err != nil || realDir(filepath.Dir(lockPath)) != nil || os.MkdirAll(filepath.Dir(lockPath), 0o700) != nil {
		return nil
	}
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return nil
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|openNoFollow, 0o600)
	if err != nil {
		return nil
	}
	defer lock.Close()
	if locked, err := tryLock(lock); err != nil || !locked {
		return nil // another refresh is under way
	}
	stop := time.AfterFunc(refreshWait, func() { os.Exit(0) })
	defer stop.Stop()

	_, expiresAt, _ := storedToken(hub)
	token, err := bearer(hub)
	if errors.Is(err, errExpired) {
		_ = updateNoticeState(refreshLockWait, func(s *noticeState) bool {
			h := s.touchHub(hub)
			h.Expired, h.TokenAt = true, expiresAt
			return true
		})
		return nil
	}
	if err != nil {
		return nil
	}
	hv := askInstalled(&hubClient{hub: hub, token: token})
	_ = updateNoticeState(refreshLockWait, func(s *noticeState) bool {
		s.recordAnswer(hub, hv, time.Now())
		return hv.status == hubOK
	})
	return nil
}

// --- The command ---

// runNotice is what the hub's session hook runs. It prints the systemMessage object or nothing, and returns nil
// whatever happens: a notice never fails a session.
func runNotice(agent, slug string, refresh bool, stdout io.Writer) error {
	if agent != claudeTarget || !validName(slug) || !validName("metro-"+slug) {
		return nil // an agent this line-lx doesn't know yet
	}
	if refresh {
		return runNoticeRefresh()
	}
	start := time.Now()
	hub, err := currentHub()
	if err != nil {
		return nil
	}
	deadline := start.Add(noticeWait)
	state := readNoticeState()
	f := noticeFacts{hub: hub, slug: slug, now: start}

	// The sign-in, without the network. A token still valid asks the hub at once; an expired one is refreshed by
	// a process of its own, and this session uses the last answer recorded.
	fromHub := make(chan hubView, 1)
	token, expiresAt, err := storedToken(hub)
	var none errNotSignedIn
	switch {
	case err == nil:
		go func() { fromHub <- askInstalled(&hubClient{hub: hub, token: token}) }()
	case errors.Is(err, errNeedsRefresh):
		_ = startRefresh(slug)
		if h := state.hub(hub); h != nil && h.Expired && h.TokenAt.Equal(expiresAt) {
			f.signIn = errExpired // the refresh it started last time was refused, and nothing signed in since
		}
	case errors.As(err, &none):
		f.signIn = err
	}
	type version struct {
		v    string
		seen *claudeSeen
	}
	fromClaude := make(chan version, 1)
	go func() {
		v, seen := claudeVersionNow(state.Claude, deadline)
		fromClaude <- version{v, seen}
	}()

	f.local, f.elsewhere, f.off = readClaudeLocal(hub, slug), marketplaceElsewhere(hub, slug), turnedOff("metro-"+slug)

	var hv hubView
	var cv version
	timeout := time.After(time.Until(deadline))
	for gotHub, gotClaude := err != nil, false; !gotHub || !gotClaude; {
		select {
		case hv = <-fromHub:
			gotHub = true
		case cv = <-fromClaude:
			gotClaude = true
		case <-timeout:
			gotHub, gotClaude = true, true // what didn't answer in time is unknown: a read, safe to leave
		}
	}
	f.claude = cv.v
	if hv.status == hubRefused {
		f.signIn = errSignInRefused{hub}
	}
	if f.answer = factsOf(hv); f.answer == nil && f.signIn == nil {
		f.answer = state.recorded(hub, slug, start)
	}

	// The plugins updated since the last session, once: remembered under a lock before they're told, and told
	// only once remembered.
	var updates, told []string
	err = updateNoticeState(noticeLockWait, func(s *noticeState) bool {
		s.recordAnswer(hub, hv, start)
		var was map[string]string
		if sp := s.space(hub, slug); sp != nil {
			was = sp.Told
		}
		lines, now := noticeUpdates(was, f)
		s.touchSpace(hub, slug, start).Told = now
		if cv.seen != nil {
			s.Claude = cv.seen
		}
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

// noticeLag is claudeLag for doctor, from the hub's answer now.
func noticeLag(hub, slug string, hv hubView, local claudeLocal, off map[string]bool) (missing, behind []string) {
	return claudeLag(noticeFacts{hub: hub, slug: slug, answer: factsOf(hv), local: local, off: off, now: time.Now()})
}
