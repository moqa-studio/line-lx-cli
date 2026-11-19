package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// --- status: what's installed on this machine from the hub, side by side with what the hub knows. ---
//
// `line-lx status` reads, for each agent line-lx knows, what this machine has from the hub: Claude Code's own
// records of its marketplaces and plugins, and the stamps line-lx wrote for Codex and pi. It puts them side
// by side with the hub's view, GET /api/me/installed: each plugin's latest version, and what the hub
// delivered to the person, and when. It writes nothing: no settings, no stamp, no lock, and runs no agent.
// Without a sign-in, or without an answer from the hub, it shows what this machine has alone.

// statusSchema is the version of `status --json`'s format. A change that can break a reader bumps it;
// adding a field doesn't.
const statusSchema = 1

// The hub's view, in statusReport.HubStatus.
const (
	hubOK          = "ok"
	hubSignedOut   = "not-signed-in"   // no sign-in for this hub on this machine
	hubRefused     = "sign-in-refused" // the hub answered 401
	hubUnreachable = "unreachable"
	hubUnsupported = "unsupported" // a hub without GET /api/me/installed (404)
	hubFailed      = "error"       // any other failure: the message says which
)

// A plugin's state, in statusPlugin.State.
const (
	pluginUpToDate  = "up-to-date"
	pluginBehind    = "behind"     // here, and the hub has a newer version
	pluginInstalled = "installed"  // here, and the hub's latest is unknown: no answer, or none for it
	pluginMissing   = "missing"    // the hub has it for this agent, this machine doesn't
	pluginNotInHub  = "not-in-hub" // here from this hub, which no longer has it
	pluginOtherHub  = "other-hub"  // here from another hub: line-lx leaves it alone
)

// statusReport is `line-lx status --json`. README.md documents it.
type statusReport struct {
	Schema     int              `json:"schema"`
	Hub        string           `json:"hub"`
	Workspace  *statusWorkspace `json:"workspace"`
	HubStatus  string           `json:"hubStatus"`
	HubMessage string           `json:"hubMessage,omitempty"`
	Agents     []statusAgent    `json:"agents"`
}

type statusWorkspace struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type statusAgent struct {
	Target      string             `json:"target"`
	Found       bool               `json:"found"`
	Connected   bool               `json:"connected"`
	Location    string             `json:"location,omitempty"` // Codex, pi: the skills folder; Claude Code: the settings file that connects it
	Problem     string             `json:"problem,omitempty"`
	Marketplace *statusMarketplace `json:"marketplace,omitempty"` // Claude Code only
	Plugins     []statusPlugin     `json:"plugins"`
}

// statusMarketplace is the hub's marketplace in Claude Code.
type statusMarketplace struct {
	Name         string     `json:"name"`
	Fetched      bool       `json:"fetched"`      // Claude Code has it in its records here: it fetched it at least once
	FetchedAt    *time.Time `json:"fetchedAt"`    // when Claude Code last fetched it, from its records here
	HubFetchedAt *time.Time `json:"hubFetchedAt"` // when the hub last saw Claude Code fetch it, from any of the person's machines
}

type statusPlugin struct {
	Name             string        `json:"name"`
	Bundle           bool          `json:"bundle"` // Claude Code's metro-lx: it brings the workspace's other plugins
	State            string        `json:"state"`
	Version          *string       `json:"version"` // installed here
	LatestVersion    *string       `json:"latestVersion"`
	Behind           bool          `json:"behind"`
	From             string        `json:"from,omitempty"`  // the hub it came from, when it isn't the current one
	Scope            string        `json:"scope,omitempty"` // Claude Code's install scope: user, project or local
	InstalledAt      *time.Time    `json:"installedAt"`     // from this machine's records
	UpdatedAt        *time.Time    `json:"updatedAt"`
	DeliveredVersion *string       `json:"deliveredVersion"` // from the hub: what it delivered to the person, on any machine
	FirstDeliveredAt *time.Time    `json:"firstDeliveredAt"`
	LastDeliveredAt  *time.Time    `json:"lastDeliveredAt"`
	Skills           []statusSkill `json:"skills"`
	Hooks            []statusHook  `json:"hooks"`
}

type statusSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type statusHook struct {
	Event   string `json:"event"`
	Command string `json:"command"`
}

// --- The hub's view ---

// hubInstalled is the hub's answer to GET /api/me/installed.
type hubInstalled struct {
	Workspace struct{ Slug, Name string } `json:"workspace"`
	Agents    []struct {
		Target        string  `json:"target"`
		LastFetchedAt *string `json:"lastFetchedAt"`
		Plugins       []struct {
			Name             string        `json:"name"`
			LatestVersion    *string       `json:"latestVersion"`
			DeliveredVersion *string       `json:"deliveredVersion"`
			FirstDeliveredAt *string       `json:"firstDeliveredAt"`
			LastDeliveredAt  *string       `json:"lastDeliveredAt"`
			Skills           []statusSkill `json:"skills"`
			Hooks            []statusHook  `json:"hooks"`
		} `json:"plugins"`
	} `json:"agents"`
}

// hubView is what status knows from the hub, checked: names are valid, text is clean and bounded.
type hubView struct {
	status, message string
	workspace       *statusWorkspace
	agents          map[string]hubAgentView // by target
	latest          map[string]*string      // every plugin the hub lists, for any agent: its latest version
}

type hubAgentView struct {
	lastFetchedAt *time.Time
	plugins       map[string]hubPluginView
}

type hubPluginView struct {
	latest, delivered *string
	first, last       *time.Time
	skills            []statusSkill
	hooks             []statusHook
}

// fetchHubView asks the hub what it delivered to the person. It never fails: a hub that can't say gives a
// view with the reason, and status then shows what this machine has alone.
func fetchHubView(hub string) hubView {
	token, err := bearer(hub)
	var none errNotSignedIn
	switch {
	case errors.As(err, &none):
		return hubView{status: hubSignedOut, message: err.Error()}
	case err != nil:
		return hubView{status: hubFailed, message: err.Error()}
	}
	api := &hubClient{hub: hub, token: token}
	res, err := api.send("GET", hub+"/api/me/installed", nil)
	var refused errSignInRefused
	var ae *apiError
	switch {
	case errors.As(err, &refused):
		return hubView{status: hubRefused, message: err.Error()}
	case errors.As(err, &ae) && ae.status == http.StatusNotFound:
		return hubView{status: hubUnsupported, message: fmt.Sprintf("The hub at %s doesn't say what it delivered yet: it needs an update.", hub)}
	case errors.As(err, &ae):
		return hubView{status: hubFailed, message: err.Error()}
	case err != nil: // send fails otherwise only when the request doesn't reach the hub
		return hubView{status: hubUnreachable, message: err.Error()}
	}
	defer res.Body.Close()
	var doc hubInstalled
	if err := json.NewDecoder(limited(res.Body)).Decode(&doc); err != nil {
		return hubView{status: hubFailed, message: fmt.Sprintf("The hub's answer to /api/me/installed isn't valid JSON: %v", err)}
	}
	return readHubView(doc)
}

// hookEvent is a Claude Code hook event's name, such as PostToolUse.
var hookEvent = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

// Bounds on the text status shows from the hub or a plugin's files.
const (
	maxDescription = 1024 // the Agent Skills spec's limit
	maxCommand     = 1000
)

// readHubView checks the hub's answer: a plugin or skill whose name isn't a valid one is dropped, a version
// that isn't one is unknown, and the text that reaches the terminal is cleaned and bounded.
func readHubView(doc hubInstalled) hubView {
	v := hubView{status: hubOK, agents: map[string]hubAgentView{}, latest: map[string]*string{}}
	if validName(doc.Workspace.Slug) {
		v.workspace = &statusWorkspace{Slug: doc.Workspace.Slug, Name: bounded(doc.Workspace.Name, 200)}
	}
	for _, a := range doc.Agents {
		if !validName(a.Target) {
			continue
		}
		if _, seen := v.agents[a.Target]; seen {
			continue // the first answer for an agent wins
		}
		agent := hubAgentView{lastFetchedAt: parseTime(a.LastFetchedAt), plugins: map[string]hubPluginView{}}
		for _, p := range a.Plugins {
			if _, seen := agent.plugins[p.Name]; seen || !validName(p.Name) {
				continue
			}
			agent.plugins[p.Name] = hubPluginView{
				latest:    checkedVersion(p.LatestVersion),
				delivered: checkedVersion(p.DeliveredVersion),
				first:     parseTime(p.FirstDeliveredAt),
				last:      parseTime(p.LastDeliveredAt),
				skills:    checkedSkills(p.Skills),
				hooks:     checkedHooks(p.Hooks),
			}
			if v.latest[p.Name] == nil { // listed, with the first version an agent gives
				v.latest[p.Name] = checkedVersion(p.LatestVersion)
			}
		}
		v.agents[a.Target] = agent
	}
	return v
}

func checkedVersion(s *string) *string {
	if s == nil || !versionName.MatchString(*s) {
		return nil
	}
	v := *s
	return &v
}

func checkedSkills(in []statusSkill) []statusSkill {
	out := []statusSkill{}
	for _, s := range in {
		if validName(s.Name) && !slices.ContainsFunc(out, func(o statusSkill) bool { return o.Name == s.Name }) {
			out = append(out, statusSkill{Name: s.Name, Description: bounded(s.Description, maxDescription)})
		}
	}
	return out
}

func checkedHooks(in []statusHook) []statusHook {
	out := []statusHook{}
	for _, h := range in {
		h = statusHook{Event: h.Event, Command: bounded(h.Command, maxCommand)}
		if hookEvent.MatchString(h.Event) && h.Command != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// bounded is s cleaned for the terminal, on one line, and cut to at most n characters.
func bounded(s string, n int) string {
	s = clean(strings.Join(strings.Fields(s), " "))
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

// parseTime reads an ISO 8601 time; anything else is unknown.
func parseTime(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// --- What this machine has ---

// localPlugin is what this machine has of one plugin, for one agent.
type localPlugin struct {
	version, hub, scope    string
	installedAt, updatedAt *time.Time
	skills                 []statusSkill
	hooks                  []statusHook
}

// localSkill is a skill from its SKILL.md, with its description when the frontmatter gives it on one line.
func localSkill(name, path string) statusSkill {
	d := frontmatterValue(path, "description")
	if strings.HasPrefix(d, ">") || strings.HasPrefix(d, "|") { // a block scalar: the text is on the next lines
		d = ""
	}
	return statusSkill{Name: name, Description: bounded(d, maxDescription)}
}

// fileLocal reads the stamps line-lx wrote for a file target, and the skills each plugin has there.
func fileLocal(t fileTarget) map[string]localPlugin {
	out := map[string]localPlugin{}
	for name, s := range t.stamps() {
		p := localPlugin{hub: s.Hub, updatedAt: parseTime(&s.SyncedAt), skills: []statusSkill{}}
		if v := checkedVersion(&s.Version); v != nil {
			p.version = *v
		}
		if t.flat {
			for _, sk := range s.Skills { // stamps() kept only valid names
				if path := filepath.Join(t.dir, name+"--"+sk, "SKILL.md"); exists(path) {
					p.skills = append(p.skills, localSkill(sk, path))
				}
			}
		} else {
			entries, _ := os.ReadDir(filepath.Join(t.dir, name))
			for _, e := range entries {
				path := filepath.Join(t.dir, name, e.Name(), "SKILL.md")
				if e.IsDir() && validName(e.Name()) && exists(path) {
					p.skills = append(p.skills, localSkill(e.Name(), path))
				}
			}
		}
		out[name] = p
	}
	return out
}

// claudeLocal is what Claude Code has from the hub on this machine.
type claudeLocal struct {
	market, where, problem string
	fetched                bool // Claude Code knows the marketplace
	fetchedAt              *time.Time
	plugins                map[string]localPlugin
}

// readClaudeLocal reads how Claude Code is connected to the hub, from its settings (as doctor does) or, for
// a marketplace declared elsewhere (an MDM profile), from its own records, and what it installed from the
// hub's marketplace: plugins/known_marketplaces.json, plugins/installed_plugins.json, and each plugin's
// folder in its cache. slug, when the hub gave it, is the person's workspace.
func readClaudeLocal(hub, slug string) claudeLocal {
	c := claudeLocal{plugins: map[string]localPlugin{}}
	var err error
	if c.market, c.where, c.problem, err = claudeConnection(hub); err != nil {
		c.problem = err.Error()
	}
	dir, err := claudeConfigDir()
	if err != nil {
		c.problem = err.Error()
		return c
	}
	plugins := filepath.Join(dir, "plugins")
	var markets map[string]struct {
		Source      struct{ Source, URL string } `json:"source"`
		LastUpdated string                       `json:"lastUpdated"`
	}
	if err := readJSONFile(filepath.Join(plugins, "known_marketplaces.json"), &markets); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.problem = fmt.Sprintf("can't read Claude Code's marketplaces: %v", err)
		return c
	}
	fromHub := func(name string) bool {
		m, ok := markets[name]
		return ok && m.Source.Source == "url" && strings.TrimRight(m.Source.URL, "/") == marketplaceURL(hub)
	}
	if c.market == "" {
		for _, name := range sortedKeys(markets) {
			if validName(name) && fromHub(name) && (c.market == "" || name == "metro-"+slug) {
				c.market = name
			}
		}
	}
	m, known := markets[c.market]
	switch {
	case c.market == "" || !known:
		return c // not connected, or Claude Code hasn't fetched the marketplace yet
	case !fromHub(c.market):
		from := m.Source.URL
		if from == "" {
			from = m.Source.Source
		}
		c.problem = fmt.Sprintf("Claude Code has the marketplace %s from %s, not from this hub", c.market, bounded(from, 200))
		return c
	}
	c.fetched, c.fetchedAt = true, parseTime(&m.LastUpdated)
	if slug != "" && c.market != "metro-"+slug && c.problem == "" {
		c.problem = fmt.Sprintf("Claude Code has the marketplace of another workspace, %s: run `line-lx sync`", c.market)
	}

	var installed struct {
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
			InstalledAt string `json:"installedAt"`
			LastUpdated string `json:"lastUpdated"`
		} `json:"plugins"`
	}
	if err := readJSONFile(filepath.Join(plugins, "installed_plugins.json"), &installed); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.problem = fmt.Sprintf("can't read Claude Code's plugins: %v", err)
		}
		return c
	}
	cache := filepath.Join(plugins, "cache")
	for _, id := range sortedKeys(installed.Plugins) {
		name, market, _ := strings.Cut(id, "@")
		entries := installed.Plugins[id]
		if market != c.market || !validName(name) || len(entries) == 0 {
			continue
		}
		// One entry per scope: the user's is the one line-lx installs.
		e := entries[0]
		for _, other := range entries {
			if other.Scope == "user" {
				e = other
				break
			}
		}
		p := localPlugin{hub: hub, scope: bounded(e.Scope, 20), installedAt: parseTime(&e.InstalledAt),
			updatedAt: parseTime(&e.LastUpdated), skills: []statusSkill{}, hooks: []statusHook{}}
		// Read files only in Claude Code's cache folder for this marketplace and plugin.
		installPath := filepath.Clean(e.InstallPath)
		rel, err := filepath.Rel(filepath.Join(cache, market, name), installPath)
		if e.InstallPath != "" && err == nil && filepath.IsLocal(rel) && !strings.Contains(rel, string(filepath.Separator)) {
			if v, ok := copyVersion(installPath); ok {
				p.version = v
			}
			entries, _ := os.ReadDir(filepath.Join(installPath, "skills"))
			for _, s := range entries {
				path := filepath.Join(installPath, "skills", s.Name(), "SKILL.md")
				if s.IsDir() && validName(s.Name()) && exists(path) {
					p.skills = append(p.skills, localSkill(s.Name(), path))
				}
			}
			p.hooks = pluginHooks(installPath)
		}
		if v := checkedVersion(&e.Version); p.version == "" && v != nil {
			p.version = *v
		}
		c.plugins[name] = p
	}
	return c
}

// pluginHooks lists the command hooks of a Claude Code plugin, from its hooks/hooks.json.
func pluginHooks(dir string) []statusHook {
	var doc struct {
		Hooks map[string][]struct {
			Hooks []statusHook `json:"hooks"`
		} `json:"hooks"`
	}
	if readJSONFile(filepath.Join(dir, "hooks", "hooks.json"), &doc) != nil {
		return []statusHook{}
	}
	var all []statusHook
	for _, event := range sortedKeys(doc.Hooks) {
		for _, group := range doc.Hooks[event] {
			for _, h := range group.Hooks {
				all = append(all, statusHook{Event: event, Command: h.Command})
			}
		}
	}
	return checkedHooks(all)
}

// --- Side by side ---

// mergePlugins puts what this machine has for an agent side by side with what the hub has for it. A plugin
// the hub lists that isn't here is missing; one here from this hub that the hub no longer lists, for any
// agent, is not in the hub.
func mergePlugins(target, hub string, local map[string]localPlugin, hv hubView) []statusPlugin {
	ha := hv.agents[target]
	names := map[string]bool{}
	for name := range local {
		names[name] = true
	}
	for name := range ha.plugins {
		names[name] = true
	}
	out := []statusPlugin{}
	for _, name := range sortedKeys(names) {
		l, here := local[name]
		h, listed := ha.plugins[name]
		latest, known := hv.latest[name]
		if listed {
			latest = h.latest
		}
		p := statusPlugin{Name: name, Bundle: target == claudeTarget && name == bundlePlugin,
			Skills: []statusSkill{}, Hooks: []statusHook{}}
		if listed {
			p.DeliveredVersion, p.FirstDeliveredAt, p.LastDeliveredAt = h.delivered, h.first, h.last
		}
		if !here {
			if latest == nil {
				continue // nothing published to install
			}
			p.State, p.LatestVersion, p.Skills, p.Hooks = pluginMissing, latest, h.skills, h.hooks
			out = append(out, p)
			continue
		}
		if l.version != "" {
			p.Version = &l.version
		}
		p.Scope, p.InstalledAt, p.UpdatedAt = l.scope, l.installedAt, l.updatedAt
		p.Skills, p.Hooks = describe(l.skills, h.skills), l.hooks
		if p.Hooks == nil {
			p.Hooks = []statusHook{}
		}
		switch {
		case l.hub != hub:
			p.State, p.From = pluginOtherHub, bounded(l.hub, 200)
		case hv.status != hubOK:
			p.State = pluginInstalled
		case !known && !p.Bundle: // the bundle isn't a plugin of the hub: an older hub doesn't list it
			p.State = pluginNotInHub
		default:
			p.LatestVersion = latest
			switch {
			case latest == nil || p.Version == nil:
				p.State = pluginInstalled
			case *latest == *p.Version:
				p.State = pluginUpToDate
			default:
				p.State, p.Behind = pluginBehind, true
			}
		}
		out = append(out, p)
	}
	// What's here first, then what's missing, by name, and the bundle last: it is there for the others.
	rank := func(p statusPlugin) int {
		switch {
		case p.Bundle:
			return 2
		case p.State == pluginMissing:
			return 1
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b statusPlugin) int { return rank(a) - rank(b) })
	return out
}

// describe gives the skills found here the hub's description when their SKILL.md has none on one line.
func describe(local, hub []statusSkill) []statusSkill {
	out := []statusSkill{}
	for _, s := range local {
		if s.Description == "" {
			if i := slices.IndexFunc(hub, func(h statusSkill) bool { return h.Name == s.Name }); i >= 0 {
				s.Description = hub[i].Description
			}
		}
		out = append(out, s)
	}
	return out
}

// buildStatus reads this machine, and puts it side by side with the hub's view, for each agent line-lx knows.
func buildStatus(hub string, hv hubView) statusReport {
	r := statusReport{Schema: statusSchema, Hub: hub, Workspace: hv.workspace, HubStatus: hv.status, HubMessage: hv.message, Agents: []statusAgent{}}

	claude := statusAgent{Target: claudeTarget, Found: claudeDetected(), Plugins: []statusPlugin{}}
	if claude.Found {
		slug := ""
		if hv.workspace != nil {
			slug = hv.workspace.Slug
		}
		c := readClaudeLocal(hub, slug)
		claude.Connected, claude.Location, claude.Problem = c.market != "", c.where, c.problem
		if c.market != "" {
			claude.Marketplace = &statusMarketplace{Name: c.market, Fetched: c.fetched, FetchedAt: c.fetchedAt,
				HubFetchedAt: hv.agents[claudeTarget].lastFetchedAt}
		}
		claude.Plugins = mergePlugins(claudeTarget, hub, c.plugins, hv)
	}
	r.Agents = append(r.Agents, claude)

	for _, t := range fileTargets() {
		a := statusAgent{Target: t.name, Found: t.detected(), Plugins: []statusPlugin{}}
		if a.Found {
			local := fileLocal(t)
			a.Location = t.dir
			for _, p := range local {
				a.Connected = a.Connected || p.hub == hub
			}
			a.Plugins = mergePlugins(t.name, hub, local, hv)
		}
		r.Agents = append(r.Agents, a)
	}
	return r
}

// --- For people ---

// statusWidth is the width the output fits in: 80 columns.
const statusWidth = 80

// painter colors text for a terminal, or leaves it plain.
type painter bool

func (p painter) paint(code, s string) string {
	if !p || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p painter) bold(s string) string   { return p.paint("1", s) }
func (p painter) dim(s string) string    { return p.paint("2", s) }
func (p painter) red(s string) string    { return p.paint("31", s) }
func (p painter) green(s string) string  { return p.paint("32", s) }
func (p painter) yellow(s string) string { return p.paint("33", s) }

// colorOn is true when f is a terminal and color is allowed.
func colorOn(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0 && colorAllowed()
}

// colorAllowed is false when NO_COLOR (https://no-color.org) or TERM=dumb is set.
func colorAllowed() bool { return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" }

// ago is a time for people: "just now", "5 min ago", "2 h ago", "3 d ago", then the date.
func ago(t *time.Time, now time.Time) string {
	if t == nil {
		return "–"
	}
	d := now.Sub(*t)
	switch {
	case d < -time.Minute || d >= 30*24*time.Hour:
		return t.In(now.Location()).Format("2006-01-02")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", d/time.Minute)
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", d/time.Hour)
	}
	return fmt.Sprintf("%d d ago", d/(24*time.Hour))
}

// fit cuts s to n characters, the last one an ellipsis when it was longer.
func fit(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string([]rune(s)[:n-1]) + "…"
}

// pad fills s with spaces to n characters (fmt's %-*s counts bytes, not characters).
func pad(s string, n int) string {
	return s + strings.Repeat(" ", max(0, n-utf8.RuneCountInString(s)))
}

// tilde writes a path under the home folder as ~/….
func tilde(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// hubNote says why status shows only what this machine has, and the error when it tells more; "" when the
// hub answered.
func hubNote(r statusReport) (note, cause string) {
	const alone = "this is only what's on this machine."
	switch r.HubStatus {
	case hubOK:
		return "", ""
	case hubSignedOut:
		return "Not signed in, so " + alone + " Run `line-lx login` to compare it with the hub.", ""
	case hubRefused:
		return "The hub didn't accept your sign-in, so " + alone + " Run `line-lx login`.", ""
	case hubUnsupported:
		return "The hub doesn't say what it delivered yet (it needs an update), so " + alone, ""
	case hubUnreachable:
		return "The hub didn't answer, so " + alone, r.HubMessage
	}
	return "The hub's answer failed, so " + alone, r.HubMessage
}

// statusText is a plugin's state for people, and its color.
func statusText(p statusPlugin, c painter) (string, func(string) string) {
	switch p.State {
	case pluginUpToDate:
		return "up to date", c.green
	case pluginBehind:
		return "behind: " + *p.LatestVersion, c.yellow
	case pluginMissing:
		return "not installed", c.yellow
	case pluginNotInHub:
		return "not in the hub", c.yellow
	case pluginOtherHub:
		return "other hub", c.dim
	}
	return "installed", func(s string) string { return s }
}

// renderStatus writes the report for people: one aligned tree per agent, in 80 columns.
func renderStatus(w io.Writer, r statusReport, now time.Time, c painter) {
	fmt.Fprintf(w, "%s %s\n", c.bold("Hub       "), r.Hub)
	switch {
	case r.Workspace != nil && r.Workspace.Name != "":
		fmt.Fprintf(w, "%s %s (%s)\n", c.bold("Workspace "), r.Workspace.Name, r.Workspace.Slug)
	case r.Workspace != nil:
		fmt.Fprintf(w, "%s %s\n", c.bold("Workspace "), r.Workspace.Slug)
	}
	if note, cause := hubNote(r); note != "" {
		fmt.Fprintln(w, c.yellow(note))
		if cause != "" {
			fmt.Fprintln(w, c.dim(cause))
		}
	}
	found := false
	for _, a := range r.Agents {
		fmt.Fprintln(w)
		renderAgent(w, a, now, c)
		found = found || a.Found
	}
	if !found {
		fmt.Fprintln(w)
		fmt.Fprintln(w, errNoAgent)
	}
}

func renderAgent(w io.Writer, a statusAgent, now time.Time, c painter) {
	title := c.bold(pad(agentName(a.Target), 13))
	var parts []string
	switch {
	case !a.Found && a.Target == claudeTarget:
		parts = append(parts, c.dim("not found (no ~/.claude, no `claude` on PATH)"))
	case !a.Found:
		parts = append(parts, c.dim("not found"))
	case a.Target == claudeTarget && a.Connected:
		parts = append(parts, c.green("connected"), a.Marketplace.Name)
		// This machine's time when Claude Code recorded one, else the hub's.
		switch fetched := firstOf(a.Marketplace.FetchedAt, a.Marketplace.HubFetchedAt); {
		case !a.Marketplace.Fetched:
			parts = append(parts, c.yellow("not fetched yet: start Claude Code"))
		case fetched != nil:
			parts = append(parts, "marketplace fetched "+ago(fetched, now))
		}
	case a.Target == claudeTarget && a.Problem == "":
		parts = append(parts, c.yellow("not connected: run `line-lx sync`"))
	case a.Target == claudeTarget:
		parts = append(parts, c.red("not connected"))
	case a.Connected:
		var last *time.Time
		for _, p := range a.Plugins {
			if p.From == "" && p.UpdatedAt != nil && (last == nil || p.UpdatedAt.After(*last)) {
				last = p.UpdatedAt
			}
		}
		parts = append(parts, c.green("set up"), tilde(a.Location))
		if last != nil {
			parts = append(parts, "synced "+ago(last, now))
		}
	default:
		parts = append(parts, c.yellow("not set up: run `line-lx sync`"), tilde(a.Location))
	}
	fmt.Fprintln(w, title+strings.Join(parts, " · "))
	if !a.Found {
		return
	}
	if a.Problem != "" {
		fmt.Fprintln(w, "  "+c.red(fit(a.Problem+".", statusWidth-2)))
	}
	if len(a.Plugins) == 0 {
		if a.Connected {
			fmt.Fprintln(w, "  "+c.dim("nothing installed from the hub yet"))
		}
		return
	}

	// The columns: as wide as their longest cell, cut so that a row fits in 80 columns.
	type row struct{ name, version, status, installed, updated string }
	rows := make([]row, len(a.Plugins))
	head := row{"PLUGIN", "VERSION", "STATUS", "INSTALLED", "UPDATED"}
	wn, wv, ws, wi := len(head.name), len(head.version), len(head.status), len(head.installed)
	for i, p := range a.Plugins {
		r := row{name: fit(p.Name, 20), version: "–", installed: "–", updated: "–"}
		if p.Version != nil {
			r.version = fit(*p.Version, 12)
		}
		r.status, _ = statusText(p, c)
		r.status = fit(r.status, 18)
		if p.State != pluginMissing { // the hub's times are the person's, on any machine: only when it is here
			r.installed, r.updated = ago(firstOf(p.InstalledAt, p.FirstDeliveredAt), now), ago(firstOf(p.UpdatedAt, p.LastDeliveredAt), now)
		}
		rows[i] = r
		wn, wv = max(wn, utf8.RuneCountInString(r.name)), max(wv, utf8.RuneCountInString(r.version))
		ws, wi = max(ws, utf8.RuneCountInString(r.status)), max(wi, utf8.RuneCountInString(r.installed))
	}
	line := func(r row, color func(string) string) string {
		return "  " + pad(r.name, wn) + "  " + pad(r.version, wv) + "  " + color(pad(r.status, ws)) + "  " + pad(r.installed, wi) + "  " + r.updated
	}
	fmt.Fprintln(w, c.dim(line(head, func(s string) string { return s })))
	var missing, behind, gone int
	for i, p := range a.Plugins {
		_, color := statusText(p, c)
		fmt.Fprintln(w, strings.TrimRight(line(rows[i], color), " "))
		renderChildren(w, p, c)
		switch p.State {
		case pluginMissing:
			missing++
		case pluginBehind:
			behind++
		case pluginNotInHub:
			gone++
		}
	}

	switch {
	case a.Target != claudeTarget && missing+behind+gone > 0:
		fmt.Fprintln(w, "  "+c.dim("Run `line-lx sync` to catch up with the hub."))
	case a.Target == claudeTarget && missing > 0:
		fmt.Fprintln(w, "  "+c.dim("Run `line-lx sync`, or start Claude Code, to install what's missing."))
	case a.Target == claudeTarget && behind+gone > 0:
		fmt.Fprintln(w, "  "+c.dim("Claude Code catches up with the hub by itself, in its next session."))
	}
}

// renderChildren writes a plugin's tree: where it came from, when that isn't the current hub, then its
// skills and its hooks, each cut to fit in 80 columns.
func renderChildren(w io.Writer, p statusPlugin, c painter) {
	type child struct{ kind, name, text string }
	var children []child
	if p.From != "" {
		children = append(children, child{"from", p.From, ""})
	}
	for _, s := range p.Skills {
		children = append(children, child{"skill", s.Name, s.Description})
	}
	for _, h := range p.Hooks {
		children = append(children, child{"hook", h.Event, h.Command})
	}
	if p.Bundle && len(p.Hooks) == 0 && p.State != pluginMissing {
		children = append(children, child{"hook", "none", ""})
	}
	wn := 0
	for _, ch := range children {
		if ch.text != "" {
			wn = max(wn, min(utf8.RuneCountInString(ch.name), 24))
		}
	}
	for i, ch := range children {
		branch := "├─ "
		if i == len(children)-1 {
			branch = "└─ "
		}
		head := "  " + branch + pad(ch.kind, 5) + " "
		if ch.text == "" {
			fmt.Fprintln(w, c.dim(head)+fit(ch.name, statusWidth-utf8.RuneCountInString(head)))
			continue
		}
		head += pad(fit(ch.name, 24), wn) + "  "
		fmt.Fprintln(w, c.dim(head)+c.dim(fit(ch.text, statusWidth-utf8.RuneCountInString(head))))
	}
}

func firstOf(times ...*time.Time) *time.Time {
	for _, t := range times {
		if t != nil {
			return t
		}
	}
	return nil
}

// runStatus shows what's installed on this machine from the hub, per agent, side by side with the hub's view.
// It changes nothing here; it exits 1 only when it can't tell which hub is the current one.
func runStatus(asJSON bool) error {
	hub, err := currentHub()
	if err != nil {
		return err
	}
	r := buildStatus(hub, fetchHubView(hub))
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	renderStatus(os.Stdout, r, time.Now(), painter(colorOn(os.Stdout)))
	return nil
}
