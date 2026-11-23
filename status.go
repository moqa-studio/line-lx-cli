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
	"unicode"
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
	Route            *int          `json:"route"`  // from the hub: its line's route number, 1–6, as the web app shows it
	Line             *string       `json:"line"`   // from the hub: its line, such as sales
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
			Route            *int          `json:"route"` // an older hub doesn't send it
			Line             *string       `json:"line"`
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
	lines           map[string]string       // the plugins the hub puts on a line: their line
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
	v := hubView{status: hubOK, agents: map[string]hubAgentView{}, latest: map[string]*string{}, lines: map[string]string{}}
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
			// A line line-lx knows, with its own route; any other has none.
			if _, seen := v.lines[p.Name]; !seen && p.Line != nil && p.Route != nil && metroLines[*p.Line].route == *p.Route {
				v.lines[p.Name] = *p.Line
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
		if line, ok := hv.lines[name]; ok && (!here || l.hub == hub) { // one from another hub isn't on this hub's map
			route := metroLines[line].route
			p.Route, p.Line = &route, &line
		}
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
//
// The output reads like the metro-lx map: each agent a terminus, each plugin a line, in the color and with
// the route number the web app gives it, and its skills and hooks the stations on it, each with its part's
// shape on its pastel. The colors are the web app's (front/src/index.css). A color never goes without its
// shape or its word, so the output says the same without color.

// statusWidth is the width the output fits in: 80 columns.
const statusWidth = 80

// rgb is a color of the web app's palette.
type rgb struct{ r, g, b uint8 }

var (
	ink         = rgb{0x12, 0x12, 0x12}
	paper       = rgb{0xff, 0xff, 0xff}
	okGreen     = rgb{0x00, 0x80, 0x1c} // --ok: up to date, connected
	skillPastel = rgb{0x9e, 0xdf, 0xfd} // --part-skill
	hookPastel  = rgb{0x9e, 0xeb, 0xc0} // --part-hook
)

// metroLine is a line as the web app draws it.
type metroLine struct {
	route int
	color rgb
	digit rgb  // its route bullet's digit: paper, or ink on the orange and the yellow
	edged bool // its stroke takes the line's darker edge: the yellow alone doesn't show on a light terminal
}

// metroLines are the lines the hub puts plugins on (GET /api/me/installed), by name.
var metroLines = map[string]metroLine{
	"engineering": {1, rgb{0x01, 0x4c, 0xc1}, paper, false},
	"product":     {2, rgb{0xb9, 0x33, 0xad}, paper, false},
	"marketing":   {3, rgb{0xf9, 0x7c, 0x2b}, ink, false},
	"sales":       {4, rgb{0xfb, 0xdc, 0x1a}, ink, true},
	"support":     {5, rgb{0x00, 0x80, 0x1c}, paper, false},
	"everyone":    {6, ink, paper, false},
}

// edge is c at 70% mixed with black: the edge of every line in the web app, which keeps the yellow visible on white.
func (c rgb) edge() rgb {
	dark := func(v uint8) uint8 { return uint8((int(v)*7 + 5) / 10) }
	return rgb{dark(c.r), dark(c.g), dark(c.b)}
}

// xterm256 is the closest of xterm's 256 colors to c: a color of the 6×6×6 cube, or one of the 24 grays.
func xterm256(c rgb) int {
	levels := [6]int{0, 95, 135, 175, 215, 255}
	dist := func(r, g, b int) int {
		dr, dg, db := int(c.r)-r, int(c.g)-g, int(c.b)-b
		return dr*dr + dg*dg + db*db
	}
	nearest := func(v uint8) int {
		best := 0
		for i, l := range levels {
			if abs(int(v)-l) < abs(int(v)-levels[best]) {
				best = i
			}
		}
		return best
	}
	r, g, b := nearest(c.r), nearest(c.g), nearest(c.b)
	best, bestDist := 16+36*r+6*g+b, dist(levels[r], levels[g], levels[b])
	for i := range 24 {
		if v := 8 + 10*i; dist(v, v, v) < bestDist {
			best, bestDist = 232+i, dist(v, v, v)
		}
	}
	return best
}

func abs(n int) int { return max(n, -n) }

// painter colors text for a terminal: with no color, 256 colors, or 24-bit color.
type painter int

const (
	noColor painter = iota
	colors256
	trueColor
)

func (p painter) paint(code, s string) string {
	if p == noColor || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p painter) bold(s string) string { return p.paint("1", s) }
func (p painter) dim(s string) string  { return p.paint("2", s) }

// color is c as a foreground (layer 38) or background (48), at p's depth.
func (p painter) color(layer int, c rgb) string {
	if p == trueColor {
		return fmt.Sprintf("%d;2;%d;%d;%d", layer, c.r, c.g, c.b)
	}
	return fmt.Sprintf("%d;5;%d", layer, xterm256(c))
}

func (p painter) fg(c rgb, s string) string { return p.paint(p.color(38, c), s) }

// on draws s in fg on bg, as a chip.
func (p painter) on(bg, fg rgb, s string) string {
	return p.paint(p.color(48, bg)+";"+p.color(38, fg), s)
}

// bullet is a plugin's sign at the head of its line, 3 columns: its route number on its line's color, the
// interchange ring for the bundle (it isn't a team's line), and nothing for a plugin the hub gives no line.
func (p painter) bullet(pl statusPlugin) string {
	l, ok := lineOf(pl)
	switch {
	case pl.Bundle:
		return " ◎ "
	case !ok:
		return "   "
	case p == noColor:
		return fmt.Sprintf("(%d)", l.route)
	case l.color == ink: // reverse video: an ink disc on a light terminal, a light one on a dark terminal
		return p.paint("1;7", fmt.Sprintf(" %d ", l.route))
	}
	return p.paint("1;"+p.color(48, l.color)+";"+p.color(38, l.digit), fmt.Sprintf(" %d ", l.route))
}

// stroke draws s in a plugin's line color; the ink line, the bundle and a plugin with no line in the
// terminal's own text color, which is ink on a light terminal and still shows on a dark one.
func (p painter) stroke(pl statusPlugin, s string) string {
	l, ok := lineOf(pl)
	switch {
	case !ok || l.color == ink:
		return s
	case l.edged:
		return p.fg(l.color.edge(), s)
	}
	return p.fg(l.color, s)
}

// part is a skill's or a hook's chip, 3 columns: the part's shape in ink on its pastel, as on the website.
func (p painter) part(kind string) string {
	if kind == "hook" {
		return p.on(hookPastel, ink, " ▲ ")
	}
	return p.on(skillPastel, ink, " ● ")
}

func lineOf(p statusPlugin) (metroLine, bool) {
	if p.Line == nil {
		return metroLine{}, false
	}
	l, ok := metroLines[*p.Line]
	return l, ok
}

// colorDepth is how status colors f: not at all when it isn't a terminal or color isn't allowed, in 24-bit
// when COLORTERM says the terminal has it, else in xterm's 256 colors.
func colorDepth(f *os.File) painter {
	info, err := f.Stat()
	switch ct := os.Getenv("COLORTERM"); {
	case err != nil || info.Mode()&os.ModeCharDevice == 0 || !colorAllowed():
		return noColor
	case ct == "truecolor" || ct == "24bit":
		return trueColor
	}
	return colors256
}

// colorAllowed is false when NO_COLOR (https://no-color.org) or TERM=dumb is set.
func colorAllowed() bool { return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" }

// ago is a time for people, as the web app says it: "just now", "5m ago", "2h ago", "yesterday", "3d ago",
// then the date.
func ago(t *time.Time, now time.Time) string {
	if t == nil {
		return "–"
	}
	d := now.Sub(*t)
	switch {
	case d < -time.Minute || d >= 7*24*time.Hour:
		return t.In(now.Location()).Format("2 Jan 2006")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", d/time.Minute)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", d/time.Hour)
	case d < 48*time.Hour:
		return "yesterday"
	}
	return fmt.Sprintf("%dd ago", d/(24*time.Hour))
}

// fit cuts s to n columns, the last one an ellipsis when it was longer.
func fit(s string, n int) string {
	if cols(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runeCols(r)
		if used+w > n-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + "…"
}

// pad fills s with spaces to n columns (fmt's %-*s counts bytes, not columns).
func pad(s string, n int) string {
	return s + strings.Repeat(" ", max(0, n-cols(s)))
}

// cols is how many columns s takes in a terminal: a wide character (CJK, most emoji) two, a combining mark none.
func cols(s string) int {
	n := 0
	for _, r := range s {
		n += runeCols(r)
	}
	return n
}

func runeCols(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me):
		return 0
	case wide(r):
		return 2
	}
	return 1
}

// wide is true for the East Asian wide and fullwidth blocks, and the emoji shown wide.
func wide(r rune) bool {
	switch {
	case r < 0x1100:
		return false
	case r <= 0x115F, // Hangul Jamo
		0x2E80 <= r && r <= 0x303E,                             // CJK radicals, symbols and punctuation
		0x3041 <= r && r <= 0x33FF,                             // kana, CJK compatibility
		0x3400 <= r && r <= 0x4DBF, 0x4E00 <= r && r <= 0x9FFF, // CJK ideographs
		0xA000 <= r && r <= 0xA4CF,                             // Yi
		0xAC00 <= r && r <= 0xD7A3,                             // Hangul syllables
		0xF900 <= r && r <= 0xFAFF,                             // CJK compatibility ideographs
		0xFE30 <= r && r <= 0xFE4F,                             // CJK compatibility forms
		0xFF00 <= r && r <= 0xFF60, 0xFFE0 <= r && r <= 0xFFE6, // fullwidth forms
		0x1F300 <= r && r <= 0x1F64F, 0x1F680 <= r && r <= 0x1F6FF, 0x1F900 <= r && r <= 0x1F9FF, // emoji
		0x1FA70 <= r && r <= 0x1FAFF,
		0x20000 <= r && r <= 0x3FFFD: // CJK extensions
		return true
	}
	return false
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

// statusText is a plugin's state in the web app's words, and how it's painted: green when up to date, grey
// when it isn't here or isn't the hub's. Nothing here has failed or is blocked, so nothing is red.
func statusText(p statusPlugin, c painter) (string, func(string) string) {
	switch p.State {
	case pluginUpToDate:
		return "up to date", func(s string) string { return c.fg(okGreen, s) }
	case pluginBehind:
		return "behind", c.bold
	case pluginMissing:
		return "not installed", c.dim
	case pluginNotInHub:
		return "not in the hub", c.dim
	case pluginOtherHub:
		return "other hub", c.dim
	}
	return "installed", func(s string) string { return s }
}

// versionText is the version here, with the hub's newer one when it's behind: "v1.0.0 → v1.1.0". The
// bundle's is a hash, without the v. "" when it isn't here.
func versionText(p statusPlugin) string {
	v := func(s *string) string {
		switch {
		case s == nil:
			return "–"
		case p.Bundle:
			return *s
		}
		return "v" + *s
	}
	switch p.State {
	case pluginMissing:
		return ""
	case pluginBehind:
		return v(p.Version) + " → " + v(p.LatestVersion)
	}
	return v(p.Version)
}

// renderStatus writes the report for people: the workspace and the hub, then each agent as a terminus with
// its plugins' lines, in 80 columns.
func renderStatus(w io.Writer, r statusReport, now time.Time, c painter) {
	head := c.bold("metro-lx")
	switch {
	case r.Workspace != nil && r.Workspace.Name != "":
		head += " · " + r.Workspace.Name + " (" + r.Workspace.Slug + ")"
	case r.Workspace != nil:
		head += " · " + r.Workspace.Slug
	}
	fmt.Fprintln(w, head)
	fmt.Fprintln(w, c.dim("hub")+"  "+strings.TrimPrefix(r.Hub, "https://"))
	if note, cause := hubNote(r); note != "" {
		fmt.Fprintln(w, note)
		if cause != "" {
			fmt.Fprintln(w, c.dim(cause))
		}
	}
	found, apart := false, true
	for _, a := range r.Agents {
		lines := agentLines(a, now, c)
		// A blank line around each agent with lines under it; the others follow one another.
		if apart || len(lines) > 1 {
			fmt.Fprintln(w)
		}
		apart = len(lines) > 1
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
		found = found || a.Found
	}
	if !found {
		fmt.Fprintln(w)
		fmt.Fprintln(w, errNoAgent)
	}
}

// agentLines is an agent for people: its terminus, how it's connected, then each plugin's line with its
// stations, and what to run to catch up with the hub.
func agentLines(a statusAgent, now time.Time, c painter) []string {
	var parts []string
	switch {
	case !a.Found && a.Target == claudeTarget:
		parts = append(parts, c.dim("not found (no ~/.claude, no `claude` on PATH)"))
	case !a.Found:
		parts = append(parts, c.dim("not found"))
	case a.Target == claudeTarget && a.Connected:
		parts = append(parts, c.fg(okGreen, "connected"))
		// This machine's time when Claude Code recorded one, else the hub's.
		switch fetched := firstOf(a.Marketplace.FetchedAt, a.Marketplace.HubFetchedAt); {
		case !a.Marketplace.Fetched:
			parts = append(parts, "not fetched yet: start Claude Code")
		case fetched != nil:
			parts = append(parts, "fetched "+ago(fetched, now))
		}
	case a.Target == claudeTarget && a.Problem == "":
		parts = append(parts, c.dim("not connected"), "run `line-lx sync`")
	case a.Target == claudeTarget:
		parts = append(parts, c.dim("not connected"))
	case a.Connected:
		var last *time.Time
		for _, p := range a.Plugins {
			if p.From == "" && p.UpdatedAt != nil && (last == nil || p.UpdatedAt.After(*last)) {
				last = p.UpdatedAt
			}
		}
		parts = append(parts, c.fg(okGreen, "set up"), tilde(a.Location))
		if last != nil {
			parts = append(parts, "synced "+ago(last, now))
		}
	default:
		parts = append(parts, c.dim("not set up"), "run `line-lx sync`")
	}
	out := []string{"▌" + c.bold(agentName(a.Target)) + strings.Repeat(" ", max(1, 14-cols(agentName(a.Target)))) + strings.Join(parts, " · ")}
	if !a.Found {
		return out
	}
	if a.Problem != "" {
		out = append(out, "  "+fit(a.Problem+".", statusWidth-2))
	}
	if len(a.Plugins) == 0 {
		if a.Connected {
			out = append(out, "  "+c.dim("nothing installed from the hub yet"))
		}
		return out
	}

	// The names line up; a version and the state follow. Names and versions are never cut: a version can be
	// the bundle's 12-character hash, or two versions.
	wn := 0
	for _, p := range a.Plugins {
		wn = max(wn, cols(p.Name))
	}
	var missing, behind, gone int
	for i, p := range a.Plugins {
		branch, under := "├─ ", "│   "
		if i == len(a.Plugins)-1 {
			branch, under = "└─ ", "    "
		}
		word, paint := statusText(p, c)
		version := versionText(p)
		row, width := c.dim(branch)+c.bullet(p)+" "+c.bold(pad(p.Name, wn)), 3+3+1+wn
		if version != "" {
			row, width = row+"  "+version, width+2+cols(version)
		}
		row, width = row+"  "+paint(word), width+2+cols(word)
		// The last update, here, else the hub's, when it fits.
		if when := ago(firstOf(p.UpdatedAt, p.LastDeliveredAt), now); p.State != pluginMissing && when != "–" && width+3+cols(when) <= statusWidth {
			row += c.dim(" · " + when)
		}
		out = append(out, c.dim("│"), row)
		out = append(out, stationLines(p, under, c)...)
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
		out = append(out, "  "+c.dim("Run `line-lx sync` to catch up with the hub."))
	case a.Target == claudeTarget && missing > 0:
		out = append(out, "  "+c.dim("Run `line-lx sync`, or start Claude Code, to install what's missing."))
	case a.Target == claudeTarget && behind+gone > 0:
		out = append(out, "  "+c.dim("Claude Code catches up with the hub by itself, in its next session."))
	}
	return out
}

// descriptionRoom is the least room a description gets on its name's line; with less, it goes under it.
const descriptionRoom = 20

// stationLines are a plugin's stations, on its line: where it came from, when that isn't the current hub,
// then its skills and its hooks, each with its part's chip. Names are whole; a description (or a hook's
// command) is cut to fit in 80 columns, on the name's line, or on the next one when a long name leaves too
// little room.
func stationLines(p statusPlugin, under string, c painter) []string {
	type station struct{ kind, name, text string }
	var stations []station
	for _, s := range p.Skills {
		stations = append(stations, station{"skill", s.Name, s.Description})
	}
	for _, h := range p.Hooks {
		stations = append(stations, station{"hook", h.Event, h.Command})
	}
	// "│   ┃ ● name": the trunk, the line, the chip.
	line := c.dim(under) + c.stroke(p, "┃")
	const nameAt = 9
	var out []string
	if p.From != "" {
		out = append(out, line+"    "+c.dim(fit("from "+p.From, statusWidth-nameAt)))
	}
	// The descriptions line up after the names, unless a name is so long the others would have no room.
	wn := 0
	for _, s := range stations {
		if n := cols(s.name); s.text != "" && nameAt+n+2+descriptionRoom <= statusWidth {
			wn = max(wn, n)
		}
	}
	for _, s := range stations {
		head := line + c.part(s.kind) + " "
		if s.text == "" {
			out = append(out, head+s.name)
			continue
		}
		if room := statusWidth - nameAt - max(wn, cols(s.name)) - 2; room >= descriptionRoom {
			out = append(out, head+pad(s.name, wn)+"  "+c.dim(fit(s.text, room)))
			continue
		}
		out = append(out, head+s.name, line+"    "+c.dim(fit(s.text, statusWidth-nameAt)))
	}
	return out
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
	renderStatus(os.Stdout, r, time.Now(), colorDepth(os.Stdout))
	return nil
}
