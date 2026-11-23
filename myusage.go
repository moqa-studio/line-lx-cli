package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
)

// --- usage: your own runs, as the web app's My usage page counts them. ---
//
// `line-lx usage` asks the hub for the person's usage (GET /api/usage/mine) and counts their runs as the web app does:
// the skills an agent reported it ran (source agent, kind activated). One bar per plugin, in its line's color and in
// the web app's order, which it takes from GET /api/me/installed; then one per agent. It writes nothing.

// runsSchema is the version of `usage --json`'s format, as statusSchema is status's.
const runsSchema = 1

// runsReport is `line-lx usage --json`. README.md documents it.
type runsReport struct {
	Schema         int          `json:"schema"`
	Hub            string       `json:"hub"`
	Period         string       `json:"period"` // week or month
	Since          *string      `json:"since"`  // the first day counted, from the hub
	AgentReporting *bool        `json:"agentReporting"`
	Runs           int          `json:"runs"`
	Plugins        []runsPlugin `json:"plugins"`
	Agents         []runsAgent  `json:"agents"`
}

type runsPlugin struct {
	Name  string  `json:"name"`
	Route *int    `json:"route"`
	Line  *string `json:"line"`
	Runs  int     `json:"runs"`
}

type runsAgent struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	Runs  int    `json:"runs"`
}

// myUsage is the hub's answer to GET /api/usage/mine: a row per plugin, version, agent, source and kind.
type myUsage struct {
	Since string `json:"since"`
	Rows  []struct {
		Plugin string `json:"plugin"`
		Agent  string `json:"agent"`
		Source string `json:"source"`
		Kind   string `json:"kind"`
		Uses   int    `json:"uses"`
	} `json:"rows"`
}

var isoDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// countRuns reads the hub's rows as the web app's "Runs reported by agents" does: source agent, kind activated. Names
// are cleaned and bounded. Plugins come in the hub's order (hv's, the web app's), then by name; agents by runs.
func countRuns(hub, period string, mine myUsage, hv hubView, reporting *bool) runsReport {
	r := runsReport{Schema: runsSchema, Hub: hub, Period: period, AgentReporting: reporting, Plugins: []runsPlugin{}, Agents: []runsAgent{}}
	if isoDay.MatchString(mine.Since) {
		r.Since = &mine.Since
	}
	plugins, agents := map[string]int{}, map[string]int{}
	for _, row := range mine.Rows {
		plugin, agent := bounded(row.Plugin, 100), bounded(row.Agent, 64)
		if row.Source != "agent" || row.Kind != "activated" || row.Uses <= 0 || plugin == "" || agent == "" {
			continue
		}
		plugins[plugin] += row.Uses
		agents[agent] += row.Uses
		r.Runs += row.Uses
	}
	for name, runs := range plugins {
		p := runsPlugin{Name: name, Runs: runs}
		if line, ok := hv.lines[name]; ok {
			route := metroLines[line].route
			p.Route, p.Line = &route, &line
		}
		r.Plugins = append(r.Plugins, p)
	}
	rank := func(name string) int {
		if i, ok := hv.order[name]; ok {
			return i
		}
		return len(hv.order)
	}
	slices.SortFunc(r.Plugins, func(a, b runsPlugin) int {
		if d := rank(a.Name) - rank(b.Name); d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})
	for id, runs := range agents {
		r.Agents = append(r.Agents, runsAgent{Agent: id, Name: agentName(id), Runs: runs})
	}
	slices.SortFunc(r.Agents, func(a, b runsAgent) int {
		if a.Runs != b.Runs {
			return b.Runs - a.Runs
		}
		return strings.Compare(a.Name, b.Name)
	})
	return r
}

// usageBars is the bars' width: the longest is 40 cells.
const usageBars = 40

// renderUsage writes the runs for people: a bar per plugin, in its line's color, then a bar per agent.
func renderUsage(w io.Writer, r runsReport, c painter) {
	days := "last 7 days"
	other := "Run `line-lx usage --month` for the last 30 days."
	if r.Period == "month" {
		days, other = "last 30 days", "Run `line-lx usage` for the last 7 days."
	}
	runs := "runs"
	if r.Runs == 1 {
		runs = "run"
	}
	writeHeader(w, c, []seg{plain("Your runs · " + days)}, []seg{{fmt.Sprint(r.Runs), c.bold}, {" " + runs, c.dim}})
	fmt.Fprintln(w)
	lines := func(segs ...seg) {
		for _, l := range paragraph(2, segs...) {
			fmt.Fprintln(w, l)
		}
	}
	off := r.AgentReporting != nil && !*r.AgentReporting
	if r.Runs == 0 {
		// The web app's words: My usage's empty state, and the settings' for agent reporting.
		if off {
			lines(plain("Agent reporting is off in this workspace, so no runs are counted."))
			lines(seg{"While it is off, nothing about usage leaves anyone's computer. An admin turns it on in Settings › General › Usage.", c.dim})
		} else {
			lines(plain("Nothing counted for you yet."))
			lines(seg{"The skills you run while agent reporting is on land here.", c.dim})
		}
		return
	}

	// The bars start after the longest name, and the counts end after the bars.
	names, digits := 16, 4
	for _, p := range r.Plugins {
		names = max(names, cols(p.Name))
		digits = max(digits, len(fmt.Sprint(p.Runs)))
	}
	for _, a := range r.Agents {
		names = max(names, cols(a.Name))
		digits = max(digits, len(fmt.Sprint(a.Runs)))
	}
	bar := 7 + names + 2
	end := bar + usageBars + 1 + digits
	most := 0
	for _, p := range r.Plugins {
		most = max(most, p.Runs)
	}
	for _, p := range r.Plugins {
		line := ""
		if p.Line != nil {
			line = *p.Line
		}
		fmt.Fprintln(w, new(row).at(2, c.badge(line, p.Name == bundlePlugin && p.Line == nil)).at(7, seg{p.Name, c.bold}).
			at(bar, seg{eighths(p.Runs, most, usageBars), c.stroke(line)}).right(end, plain(fmt.Sprint(p.Runs))).String())
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, new(row).at(2, seg{"BY AGENT", c.dim}).String())
	fmt.Fprintln(w)
	most = 0
	for _, a := range r.Agents {
		most = max(most, a.Runs)
	}
	for _, a := range r.Agents {
		cells := max(1, int(float64(a.Runs)/float64(most)*usageBars+0.5))
		fmt.Fprintln(w, new(row).at(7, plain(a.Name)).at(bar, plain(strings.Repeat("━", cells))).right(end, plain(fmt.Sprint(a.Runs))).String())
	}

	fmt.Fprintln(w)
	lines(seg{"Only your own runs. Admins see the workspace's totals in the web app.", c.dim})
	if off {
		lines(seg{"Agent reporting is off now in this workspace: new runs aren't counted.", c.dim})
	}
	lines(seg{other, c.dim})
}

// runUsage shows the person's own runs over the last 7 days, or 30 with month. It needs a sign-in; it writes nothing.
func runUsage(month, asJSON bool) error {
	api, err := newHubClient() // without a sign-in, its error says to run `line-lx login`
	if err != nil {
		return err
	}
	period := "week"
	if month {
		period = "month"
	}
	var mine myUsage
	if err := api.do("GET", "/api/usage/mine?period="+period, nil, &mine); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusNotFound {
			return fmt.Errorf("The hub at %s doesn't count usage yet: it needs an update.", api.hub)
		}
		return err
	}
	// The lines and the web app's order, and whether agent reporting is on: without them, the runs still show.
	hv := askInstalled(api)
	var settings struct {
		AgentReporting *bool `json:"agentReporting"`
	}
	_ = api.do("GET", "/api/usage/settings", nil, &settings)

	r := countRuns(api.hub, period, mine, hv, settings.AgentReporting)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	renderUsage(os.Stdout, r, colorDepth(os.Stdout))
	return nil
}
