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
	person     string           // who is signed in, from the hub: for people only
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
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Review      *statusReview `json:"review"` // from the hub: the security review of the version it delivered
}

// statusReview is a skill's security review, as the web app's badge says it.
type statusReview struct {
	Status string       `json:"status"` // pending, done or failed
	Label  string       `json:"label"`  // the web app's words, such as "No findings" or "To review"
	Tone   string       `json:"tone"`   // ok, neutral, attention or danger
	Counts reviewCounts `json:"counts"`
}

// reviewCounts are a review's findings, per severity.
type reviewCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
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
			Name             string       `json:"name"`
			Route            *int         `json:"route"` // an older hub doesn't send it
			Line             *string      `json:"line"`
			LatestVersion    *string      `json:"latestVersion"`
			DeliveredVersion *string      `json:"deliveredVersion"`
			FirstDeliveredAt *string      `json:"firstDeliveredAt"`
			LastDeliveredAt  *string      `json:"lastDeliveredAt"`
			Skills           []hubSkill   `json:"skills"`
			Hooks            []statusHook `json:"hooks"`
		} `json:"plugins"`
	} `json:"agents"`
}

// hubSkill is a skill in the hub's answer. Its review is read on its own: one in a shape line-lx doesn't know (an
// older or newer hub) is dropped, never a reason to refuse the whole answer.
type hubSkill struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Review      json.RawMessage `json:"review"`
}

// hubView is what status knows from the hub, checked: names are valid, text is clean and bounded.
type hubView struct {
	status, message string
	person          string // who is signed in, when the hub says
	workspace       *statusWorkspace
	agents          map[string]hubAgentView // by target
	latest          map[string]*string      // every plugin the hub lists, for any agent: its latest version
	lines           map[string]string       // the plugins the hub puts on a line: their line
	order           map[string]int          // every plugin the hub lists: its place in the hub's order, the web app's
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
	// Who is signed in, for the header, at the same time: without the hub's answer, the header leaves it out.
	person := make(chan string, 1)
	go func() {
		var m me
		if api.do("GET", "/api/me", nil, &m) != nil {
			m.User.Name = ""
		}
		person <- bounded(m.User.Name, 100)
	}()
	v := askInstalled(api)
	if v.status == hubOK {
		v.person = <-person
	}
	return v
}

// askInstalled asks the hub what it delivered to the person, with api's sign-in (GET /api/me/installed). It never
// fails: a hub that can't say gives a view with the reason.
func askInstalled(api *hubClient) hubView {
	res, err := api.send("GET", api.hub+"/api/me/installed", nil)
	var refused errSignInRefused
	var ae *apiError
	switch {
	case errors.As(err, &refused):
		return hubView{status: hubRefused, message: err.Error()}
	case errors.As(err, &ae) && ae.status == http.StatusNotFound:
		return hubView{status: hubUnsupported, message: fmt.Sprintf("The hub at %s doesn't say what it delivered yet: it needs an update.", api.hub)}
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
	maxLabel       = 40 // a review's label: the web app's badges say it in a few words
)

// readHubView checks the hub's answer: a plugin or skill whose name isn't a valid one is dropped, a version
// that isn't one is unknown, and the text that reaches the terminal is cleaned and bounded.
func readHubView(doc hubInstalled) hubView {
	v := hubView{status: hubOK, agents: map[string]hubAgentView{}, latest: map[string]*string{}, lines: map[string]string{},
		order: map[string]int{}}
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
			if _, seen := v.order[p.Name]; !seen {
				v.order[p.Name] = len(v.order)
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

func checkedSkills(in []hubSkill) []statusSkill {
	out := []statusSkill{}
	for _, s := range in {
		if validName(s.Name) && !slices.ContainsFunc(out, func(o statusSkill) bool { return o.Name == s.Name }) {
			out = append(out, statusSkill{Name: s.Name, Description: bounded(s.Description, maxDescription), Review: checkedReview(s.Review)})
		}
	}
	return out
}

// checkedReview reads a skill's review from the hub: a status or a tone line-lx doesn't know, an empty label or any
// other shape is no review. The label is the web app's words, cleaned and bounded; a count below zero is zero.
func checkedReview(raw json.RawMessage) *statusReview {
	var r statusReview
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil {
		return nil
	}
	r.Label = bounded(r.Label, maxLabel)
	if !slices.Contains([]string{"pending", "done", "failed"}, r.Status) ||
		!slices.Contains([]string{"ok", "neutral", "attention", "danger"}, r.Tone) || r.Label == "" {
		return nil
	}
	for _, n := range []*int{&r.Counts.Critical, &r.Counts.High, &r.Counts.Medium, &r.Counts.Low} {
		*n = max(0, *n)
	}
	return &r
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
	// The hub's plugins in its order, the web app's (so by route), then the ones it doesn't have, by name, and the
	// bundle last: it is there for the others.
	rank := func(p statusPlugin) int {
		i, known := hv.order[p.Name]
		switch {
		case p.Bundle:
			return len(hv.order) + 1
		case !known || p.State == pluginNotInHub || p.State == pluginOtherHub:
			return len(hv.order)
		}
		return i
	}
	slices.SortStableFunc(out, func(a, b statusPlugin) int { return rank(a) - rank(b) })
	return out
}

// describe gives the skills found here the hub's description when their SKILL.md has none on one line, and the
// hub's review of the skill of that name.
func describe(local, hub []statusSkill) []statusSkill {
	out := []statusSkill{}
	for _, s := range local {
		if i := slices.IndexFunc(hub, func(h statusSkill) bool { return h.Name == s.Name }); i >= 0 {
			if s.Description == "" {
				s.Description = hub[i].Description
			}
			s.Review = hub[i].Review
		}
		out = append(out, s)
	}
	return out
}

// buildStatus reads this machine, and puts it side by side with the hub's view, for each agent line-lx knows.
func buildStatus(hub string, hv hubView) statusReport {
	r := statusReport{Schema: statusSchema, Hub: hub, Workspace: hv.workspace, HubStatus: hv.status, HubMessage: hv.message,
		Agents: []statusAgent{}, person: hv.person}

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
// One card per agent found here: each plugin with its line's badge, its version, its state and its last update, then
// its skills, each with its security review, and its hooks. Above the cards, how much is up to date, counted per plugin
// and agent; under them, the agents that aren't here. Descriptions and hook commands stay in --json.

// ago is a time for people, as the web app says it: "just now", "5m ago", "2h ago", "yesterday", "3d ago",
// then the date. "—" when unknown.
func ago(t *time.Time, now time.Time) string {
	if t == nil {
		return "—"
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

// stateSeg is a plugin's state in the web app's words: green when up to date, bold when behind, dim when it isn't
// here or isn't the hub's. Nothing here has failed or is blocked, so nothing is red.
func stateSeg(p statusPlugin, c painter) seg {
	switch p.State {
	case pluginUpToDate:
		return seg{"up to date", c.green}
	case pluginBehind:
		return seg{"behind", c.bold}
	case pluginMissing:
		return seg{"not installed", c.dim}
	case pluginNotInHub:
		return seg{"not in the hub", c.dim}
	case pluginOtherHub:
		return seg{"other hub", c.dim}
	}
	return seg{"installed", c.dim}
}

// versionSegs is the version here, with the hub's newer one in bold when it's behind: "v1.0.0 → v1.1.0". The
// bundle's is a hash, without the v. "—" when it isn't here.
func versionSegs(p statusPlugin, c painter) []seg {
	v := func(s *string) string {
		switch {
		case s == nil:
			return "—"
		case p.Bundle:
			return *s
		}
		return "v" + *s
	}
	switch p.State {
	case pluginMissing:
		return []seg{{"—", c.dim}}
	case pluginBehind:
		return []seg{plain(v(p.Version)), {" → ", c.dim}, {v(p.LatestVersion), c.bold}}
	}
	return []seg{{v(p.Version), c.dim}}
}

// dateOf is a plugin's last update: this machine's, else the hub's.
func dateOf(p statusPlugin, now time.Time) string {
	if p.State == pluginMissing {
		return "—"
	}
	return ago(firstOf(p.UpdatedAt, p.LastDeliveredAt), now)
}

// reviewSegs is a skill's security review: its tone's mark, the web app's label as it is, and the worst findings when
// they're at medium or above, as in "! To review · 1 medium". Nothing without a review.
func reviewSegs(r *statusReview, c painter) []seg {
	if r == nil {
		return nil
	}
	mark, label := seg{"·", c.dim}, seg{" " + r.Label, c.dim}
	switch r.Tone {
	case "ok":
		mark = seg{"✓", c.green}
	case "attention":
		mark, label.paint = seg{"!", c.bold}, nil
	case "danger":
		mark, label.paint = seg{"✗", c.red}, nil
	}
	segs := []seg{mark, label}
	for _, worst := range []struct {
		n    int
		name string
	}{{r.Counts.Critical, "critical"}, {r.Counts.High, "high"}, {r.Counts.Medium, "medium"}} {
		if worst.n > 0 {
			return append(segs, seg{fmt.Sprintf(" · %d %s", worst.n, worst.name), c.dim})
		}
	}
	return segs
}

// tally counts the plugins of the agents here, once per plugin and agent. Another hub's, and one whose latest
// version isn't known, aren't the hub's to count.
type tally struct{ upToDate, behind, missing, gone int }

func (t tally) total() int { return t.upToDate + t.behind + t.missing + t.gone }

func countPlugins(r statusReport) tally {
	var t tally
	for _, a := range r.Agents {
		for _, p := range a.Plugins {
			switch p.State {
			case pluginUpToDate:
				t.upToDate++
			case pluginBehind:
				t.behind++
			case pluginMissing:
				t.missing++
			case pluginNotInHub:
				t.gone++
			}
		}
	}
	return t
}

// progressRow is how much is up to date: a line of 26 cells, green for the part up to date, then the counts. The line
// gets shorter when the counts need the room.
func progressRow(t tally, c painter) string {
	count := []seg{{fmt.Sprintf("%d of %d", t.upToDate, t.total()), c.bold}, plain(" up to date")}
	var rest []string
	for _, part := range []struct {
		n    int
		word string
	}{{t.behind, "behind"}, {t.missing, "not installed"}, {t.gone, "not in the hub"}} {
		if part.n > 0 {
			rest = append(rest, fmt.Sprintf("%d %s", part.n, part.word))
		}
	}
	if len(rest) > 0 {
		count = append(count, seg{" · " + strings.Join(rest, " · "), c.dim})
	}
	cells := min(26, max(8, screenEnd-2-2-segsWidth(count)))
	done := int(float64(t.upToDate)/float64(t.total())*float64(cells) + 0.5)
	return new(row).at(2, seg{strings.Repeat("━", done), c.green}, seg{strings.Repeat("─", cells-done), c.dim}, plain("  ")).
		add(count...).String()
}

// statusLayout is where each column starts inside the cards, the same in every card: the badge, the plugin's name,
// its version, its parts (skills and hooks), its state, and the inside's width, where the last update ends. Names and
// versions are never cut: a long one moves the columns after it, and widens the cards.
type statusLayout struct {
	badges                            bool // the hub gave the lines: without it, the badge column is left out
	name, version, part, state, inner int
}

func layoutStatus(r statusReport, now time.Time) statusLayout {
	l := statusLayout{badges: r.HubStatus == hubOK}
	if l.badges {
		l.name = 5
	}
	l.part = l.name + 4
	var names, versions, states, dates int
	for _, a := range r.Agents {
		if a.Found {
			for _, p := range a.Plugins {
				names = max(names, cols(p.Name))
				versions = max(versions, segsWidth(versionSegs(p, noColor)))
				states = max(states, cols(stateSeg(p, noColor).text))
				dates = max(dates, cols(dateOf(p, now)))
			}
		}
	}
	l.version = l.name + names + 2
	// The states line up after the versions, unless a long one (two of the bundle's hashes) would push them out of
	// 80 columns: then that row's state alone moves.
	l.state = max(l.name+35, min(l.version+versions+3, cardInner-states-2-dates))
	l.inner = cardInner
	// A part's review goes in the state column, or two columns after a name too long for it.
	for _, a := range r.Agents {
		if !a.Found {
			continue
		}
		l.inner = max(l.inner, cols(agentName(a.Target))+segsWidth(agentState(a, now, noColor))+3)
		for _, p := range a.Plugins {
			l.inner = max(l.inner, l.stateAt(p)+cols(stateSeg(p, noColor).text)+2+cols(dateOf(p, now)))
			for _, s := range p.Skills {
				l.inner = max(l.inner, l.partEnd(s.Name)+segsWidth(reviewSegs(s.Review, noColor)))
			}
			for _, h := range p.Hooks {
				l.inner = max(l.inner, l.partEnd(h.Event))
			}
		}
	}
	return l
}

// stateAt is where a plugin's state starts: in the state column, or three columns after a version too long for it.
func (l statusLayout) stateAt(p statusPlugin) int {
	return max(l.state, l.version+segsWidth(versionSegs(p, noColor))+3)
}

// partEnd is where what's said of a part (its review, what a hook runs) starts: in the state column, or after its name.
func (l statusLayout) partEnd(name string) int { return max(l.state, l.part+cols(name)+2) }

// agentState is how an agent is connected, for its card's top border.
func agentState(a statusAgent, now time.Time, c painter) []seg {
	switch {
	case a.Target == claudeTarget && a.Connected:
		out := []seg{{"connected", c.green}}
		// This machine's time when Claude Code recorded one, else the hub's.
		switch fetched := firstOf(a.Marketplace.FetchedAt, a.Marketplace.HubFetchedAt); {
		case !a.Marketplace.Fetched:
			out = append(out, plain(" · not fetched yet: start Claude Code"))
		case fetched != nil:
			out = append(out, plain(" · fetched "+ago(fetched, now)))
		}
		return out
	case a.Target == claudeTarget && a.Problem != "":
		return []seg{{"not connected", c.dim}}
	case a.Target == claudeTarget:
		return []seg{{"not connected", c.dim}, plain(" · run `line-lx sync`")}
	case a.Connected:
		var last *time.Time
		for _, p := range a.Plugins {
			if p.From == "" && p.UpdatedAt != nil && (last == nil || p.UpdatedAt.After(*last)) {
				last = p.UpdatedAt
			}
		}
		out := []seg{{"set up", c.green}}
		if last != nil {
			out = append(out, plain(" · synced "+ago(last, now)))
		}
		return out
	}
	return []seg{{"not set up", c.dim}, plain(" · run `line-lx sync`")}
}

// agentCard is an agent's card: each plugin, a blank line apart, with its skills and hooks under its name, then what's
// wrong with the connection and what to run to catch up with the hub.
func agentCard(a statusAgent, l statusLayout, now time.Time, c painter) card {
	k := card{title: agentName(a.Target), state: agentState(a, now, c)}
	var missing, behind, gone int
	for i, p := range a.Plugins {
		if i > 0 {
			k.rows = append(k.rows, new(row))
		}
		head := new(row)
		if l.badges {
			line := ""
			if p.Line != nil {
				line = *p.Line
			}
			head.at(0, c.badge(line, p.Bundle))
		}
		head.at(l.name, seg{p.Name, c.bold}).at(l.version, versionSegs(p, c)...).at(l.stateAt(p), stateSeg(p, c))
		k.rows = append(k.rows, head.right(l.inner, seg{dateOf(p, now), c.dim}))
		if p.From != "" {
			k.rows = append(k.rows, new(row).at(l.part, seg{fit("from "+p.From, l.inner-l.part), c.dim}))
		}
		for _, s := range p.Skills {
			k.rows = append(k.rows, new(row).at(l.name, c.chip("skill")).add(plain(" "+s.Name)).at(l.partEnd(s.Name), reviewSegs(s.Review, c)...))
		}
		for _, h := range p.Hooks {
			k.rows = append(k.rows, new(row).at(l.name, c.chip("hook")).add(plain(" "+h.Event)).
				at(l.partEnd(h.Event), seg{fitWords("runs "+h.Command, l.inner-l.partEnd(h.Event)), c.dim}))
		}
		switch p.State {
		case pluginMissing:
			missing++
		case pluginBehind:
			behind++
		case pluginNotInHub:
			gone++
		}
	}

	var notes [][]seg
	if a.Problem != "" {
		notes = append(notes, []seg{plain(a.Problem + ".")})
	}
	switch {
	case len(a.Plugins) == 0:
		notes = append(notes, []seg{{"Nothing installed from the hub yet.", c.dim}})
	case a.Target != claudeTarget && missing+behind+gone > 0:
		notes = append(notes, []seg{{"Run `line-lx sync` to catch up with the hub.", c.dim}})
	case a.Target == claudeTarget && missing > 0:
		notes = append(notes, []seg{{"Run `line-lx sync`, or start Claude Code, to install what's missing.", c.dim}})
	case a.Target == claudeTarget && behind+gone > 0:
		notes = append(notes, []seg{{"Claude Code catches up with the hub by itself, in its next session.", c.dim}})
	}
	if len(notes) > 0 && len(a.Plugins) > 0 {
		k.rows = append(k.rows, new(row))
	}
	for _, n := range notes {
		for _, line := range flow(n, l.inner) {
			k.rows = append(k.rows, new(row).add(line...))
		}
	}
	return k
}

// renderStatus writes the report for people: the workspace, who is signed in and the hub, how much is up to date,
// then a card per agent here, and the agents that aren't.
func renderStatus(w io.Writer, r statusReport, now time.Time, c painter) {
	var title, right []seg
	if r.Workspace != nil {
		name := r.Workspace.Name
		if name == "" {
			name = r.Workspace.Slug
		}
		title = []seg{plain(name)}
	}
	if r.person != "" {
		right = []seg{{"signed in as " + r.person, c.dim}}
	}
	writeHeader(w, c, title, right)
	fmt.Fprintln(w, new(row).at(6, seg{"hub", c.dim}).add(plain("  "+hostOf(r.Hub))).String())
	if note, cause := hubNote(r); note != "" {
		fmt.Fprintln(w)
		for _, line := range paragraph(2, plain(note)) {
			fmt.Fprintln(w, line)
		}
		if cause != "" {
			for _, line := range paragraph(2, seg{cause, c.dim}) {
				fmt.Fprintln(w, line)
			}
		}
	}
	if t := countPlugins(r); r.HubStatus == hubOK && t.total() > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, progressRow(t, c))
	}

	l := layoutStatus(r, now)
	var cards []card
	var away []statusAgent
	for _, a := range r.Agents {
		if a.Found {
			cards = append(cards, agentCard(a, l, now, c))
		} else {
			away = append(away, a)
		}
	}
	if len(cards) > 0 {
		fmt.Fprintln(w)
		renderCards(w, c, cards)
	}
	if len(away) > 0 {
		fmt.Fprintln(w)
		wn := 0
		for _, a := range away {
			wn = max(wn, cols(agentName(a.Target)))
		}
		for _, a := range away {
			fmt.Fprintln(w, new(row).at(2, seg{"○", c.dim}).at(5, seg{agentName(a.Target), c.bold}).at(5+wn+2, seg{"not found", c.dim}).String())
		}
	}
	if len(cards) == 0 {
		fmt.Fprintln(w)
		for _, line := range paragraph(2, plain(errNoAgent.Error())) {
			fmt.Fprintln(w, line)
		}
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
	renderStatus(os.Stdout, r, time.Now(), colorDepth(os.Stdout))
	return nil
}
