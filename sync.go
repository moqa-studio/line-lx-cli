package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const claudeTarget = "claude-code"

var errNoAgent = errors.New("No agent found on this machine (looked for ~/.codex, ~/.pi/agent, and ~/.claude or `claude` on PATH).")

// agentResult is what a sync did for one agent.
type agentResult struct {
	name, where                string
	delivered, current, failed int
	removed                    int              // plugins line-lx took out of the agent
	changes                    []pluginChange   // Codex and pi: what changed, plugin by plugin
	claude                     *claudeConnected // Claude Code: what connecting it did
	outdated                   string           // Claude Code: its version, when it's older than minClaudeVersion
	skipped                    string           // why the agent got nothing, when sync skipped it
	err                        string           // why the agent failed as a whole
}

// pluginChange is one plugin line-lx wrote to an agent, or took out of it.
type pluginChange struct {
	name, version string
	from          string // the version it replaced; "" when the agent didn't have it
	action        string // added, updated, removed or failed
	detail        string // why it failed, or what the agent couldn't take
}

func runSync() error {
	api, err := newHubClient()
	if err != nil {
		return err
	}
	// The workspace's name, for the header: without it, the header has none.
	title := []seg{plain("sync")}
	var m me
	if api.do("GET", "/api/me", nil, &m) == nil && clean(m.Org.Name) != "" {
		title = []seg{plain("sync · " + clean(m.Org.Name))}
	}
	writeHeader(os.Stdout, colorDepth(os.Stdout), title, nil)
	fmt.Println()
	_, err = syncAll(api)
	return err
}

// syncAll connects Claude Code to the hub, which then gets the plugins by itself, writes the hub's plugins to Codex
// and pi, and reports each delivery to the hub. It prints a row per agent as it goes, with a spinner on a terminal. It
// returns errNoAgent when there's no agent, and errSilent when a target failed (the details are printed).
func syncAll(api *hubClient) ([]*agentResult, error) {
	start := time.Now()
	c := colorDepth(os.Stdout)
	unlock, err := syncLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	rels, err := api.releases()
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		printLines(paragraph(2, seg{"Your workspace has no published plugins yet.", c.dim}))
		fmt.Println()
	}
	// The plugins' lines, for their badges: a hub that doesn't say leaves them out.
	lines := askInstalled(api).lines

	var results []*agentResult
	var failed int
	var reportErr error
	show := func(res *agentResult) {
		results = append(results, res)
		failed += res.failed
		printLines(syncRows(res, lines, c))
	}
	away := func(target string) { printLines(checkRow(c, checkNone, agentName(target), seg{"not found", c.dim})) }

	// Claude Code: line-lx writes no plugin, it connects Claude Code to the hub. Nothing is reported to the
	// hub: Claude Code fetches the plugins itself.
	if claudeDetected() {
		s := spin(os.Stdout, c, c != noColor, agentName(claudeTarget))
		s.say("connecting to the hub")
		res := &agentResult{name: claudeTarget}
		done, err := claudeConnect(api)
		switch {
		case claudeSkipped(err):
			res.skipped = err.Error()
		case err != nil:
			res.err, res.failed = err.Error(), 1
		default:
			res.claude, res.outdated = &done, claudeOutdated()
		}
		s.stop()
		show(res)
	} else {
		away(claudeTarget)
	}

	inHub := map[string]bool{}
	for _, r := range rels {
		inHub[r.name] = true
	}
	home, _ := os.UserHomeDir()
	for _, t := range fileTargets() {
		if !t.detected() {
			away(t.name)
			continue
		}
		res := &agentResult{name: t.name, where: t.dir}
		s := spin(os.Stdout, c, c != noColor, agentName(t.name))
		if err := t.sweepStaging(); err != nil { // leftovers of an interrupted sync
			res.err, res.failed = err.Error(), 1
			s.stop()
			show(res)
			continue
		}
		stamps := t.stamps()
		for _, r := range rels {
			before, had := stamps[r.name]
			if had && before.Hub == api.hub && before.VersionID == r.version.ID {
				res.current++
				continue
			}
			s.say("writing " + clean(r.name) + " v" + clean(r.version.Version))
			unsupported, werr := t.write(api.hub, r)
			ch := pluginChange{name: clean(r.name), version: clean(r.version.Version), action: "added"}
			if had && before.Hub == api.hub {
				ch.action, ch.from = "updated", clean(before.Version)
			}
			status, errMsg := "delivered", ""
			switch {
			case werr != nil:
				// The hub gets paths relative to the home folder, not this machine's layout.
				errMsg = werr.Error()
				if home != "" {
					errMsg = strings.ReplaceAll(errMsg, home, "~")
				}
				status, ch.action, ch.detail = "failed", "failed", werr.Error()
				res.failed++
			case len(unsupported) > 0:
				status, ch.detail = "partial", "skipped "+strings.Join(unsupported, "; ")
				res.delivered++
			default:
				res.delivered++
			}
			res.changes = append(res.changes, ch)
			if err := api.report(r.version.ID, t.name, status, unsupported, errMsg); err != nil {
				reportErr = err
			}
		}
		// Plugins this hub no longer has: only folders carrying our stamp, and only this hub's.
		for _, name := range sortedKeys(stamps) {
			st := stamps[name]
			if st.Hub != api.hub || inHub[name] {
				continue
			}
			s.say("removing " + name)
			ch := pluginChange{name: name, version: clean(st.Version), action: "removed"}
			if err := t.remove(name, st); err != nil {
				ch.action, ch.detail = "failed", "can't remove it: "+err.Error()
				res.failed++
			} else {
				res.removed++
			}
			res.changes = append(res.changes, ch)
		}
		_ = t.sweepStaging() // nothing left behind once this target is done
		s.stop()
		show(res)
	}

	if len(results) == 0 {
		return nil, errNoAgent
	}
	if reportErr != nil {
		printLines(checkRow(c, checkFail, "Hub", plain("Couldn't report every delivery to the hub: "+reportErr.Error())))
	}
	fmt.Println()
	fmt.Println(doneRow(failed, time.Since(start), c))
	if failed > 0 || reportErr != nil {
		return results, errSilent{}
	}
	return results, nil
}

// syncRows is what a sync did for one agent, for people: its check row, the plugins it changed, and the next step.
func syncRows(r *agentResult, lines map[string]string, c painter) []string {
	name := agentName(r.name)
	var out []string
	add := func(rows []string) { out = append(out, rows...) }
	switch {
	case r.skipped != "":
		add(checkRow(c, checkNone, name, plain("skipped: "+r.skipped+".")))
	case r.err != "":
		add(checkRow(c, checkFail, name, plain(r.err)))
	case r.claude != nil:
		how := "it gets the plugins from its next start"
		if r.claude.now {
			how = "it installs new versions itself"
		}
		add(checkRow(c, checkOK, name, plain("connected"), seg{" · " + how, c.dim}))
		if r.claude.reload {
			add(fixRow(c, checkCol, "Run /reload-plugins in Claude Code, or restart it."))
		}
		if r.claude.comeBack {
			add(noteRow(c, checkCol, "The plugins line-lx installed before come back when Claude Code starts again."))
		}
		if len(r.claude.pending) > 0 {
			add(noteRow(c, checkCol, fmt.Sprintf("It installs %s at its next start instead (%v).", strings.Join(r.claude.pending, ", "), r.claude.pendingErr)))
		}
		for _, p := range r.claude.refused {
			add(noteRow(c, checkCol, fmt.Sprintf("Skipped the plugin %q: line-lx refuses its name.", p)))
		}
	default:
		kind, detail := checkOK, []seg{plain(plural(r.delivered+r.current, "plugin")), {" · " + tilde(r.where), c.dim}}
		if r.failed > 0 {
			kind = checkFail
		}
		if len(r.changes) == 0 {
			detail = append(detail, seg{" · nothing new", c.dim})
		}
		add(checkRow(c, kind, name, detail...))
		add(changeRows(r.changes, lines, c))
		if next := nextStep(r.name); next != "" && r.delivered+r.removed > 0 {
			add(fixRow(c, checkCol, next))
		}
	}
	if r.outdated != "" {
		add(checkRow(c, checkWarn, name, plain(outdatedText(r.outdated))))
		add(fixRow(c, checkCol, claudeUpdate))
	}
	return out
}

// changeRows are the plugins a sync changed in an agent, under its row: each with its line's badge, its version,
// and what happened to it.
func changeRows(changes []pluginChange, lines map[string]string, c painter) []string {
	badges := false
	for _, ch := range changes {
		_, ok := lines[ch.name]
		badges = badges || ok
	}
	at := checkCol
	if badges {
		at += 5
	}
	var names, versions int
	for _, ch := range changes {
		names, versions = max(names, cols(ch.name)), max(versions, cols("v"+ch.version))
	}
	version := at + max(18, names+2)
	action := version + versions + 3
	var out []string
	for _, ch := range changes {
		r := new(row)
		if badges {
			r.at(checkCol, c.badge(lines[ch.name], false))
		}
		r.at(at, seg{ch.name, c.bold}).at(version, plain("v"+ch.version))
		switch {
		case ch.action == "added":
			r.at(action, seg{"added", c.green})
		case ch.action == "updated" && ch.from != "" && ch.from != ch.version:
			r.at(action, plain("updated"), seg{" from v" + ch.from, c.dim})
		case ch.action == "removed":
			r.at(action, plain("removed"), seg{" · not in the hub anymore", c.dim})
		case ch.action == "failed":
			r.at(action, seg{"failed", c.red})
		default:
			r.at(action, plain(ch.action))
		}
		out = append(out, r.String())
		if ch.detail != "" {
			out = append(out, noteRow(c, at, ch.detail)...)
		}
	}
	return out
}

// doneRow ends a sync: how long it took, and how many deliveries failed.
func doneRow(failed int, took time.Duration, c painter) string {
	done := fmt.Sprintf("Done in %.1fs.", took.Seconds())
	if failed == 0 {
		return new(row).at(2, plain(done)).String()
	}
	return new(row).at(2, seg{fmt.Sprintf("%d failed.", failed), c.red}, plain(" "+done)).String()
}

// installedPlugin is what a target has for one plugin; versionID is empty for Claude Code.
type installedPlugin struct {
	version, versionID, hub string
}

const stateCurrent = "up to date"

// hubState compares an installed plugin with the hub's latest. rels is nil when the hub didn't answer.
func hubState(hub string, rels map[string]release, name string, p installedPlugin) string {
	if p.hub != hub {
		return "from " + p.hub
	}
	if rels == nil {
		return ""
	}
	r, ok := rels[name]
	switch {
	case !ok:
		return "not in the hub anymore"
	case p.versionID == r.version.ID || p.versionID == "" && p.version == r.version.Version:
		return stateCurrent
	default:
		return "update available (" + clean(r.version.Version) + ")"
	}
}

func fileInstalled(t fileTarget) map[string]installedPlugin {
	out := map[string]installedPlugin{}
	for name, s := range t.stamps() {
		out[name] = installedPlugin{version: s.Version, versionID: s.VersionID, hub: s.Hub}
	}
	return out
}

// hubReleases returns the hub's latest releases by plugin name, for list and doctor.
func hubReleases(api *hubClient) (map[string]release, error) {
	rels, err := api.releases()
	if err != nil {
		return nil, err
	}
	out := map[string]release{}
	for _, r := range rels {
		out[r.name] = r
	}
	return out, nil
}

func runList() error {
	hub, err := currentHub()
	if err != nil {
		return err
	}
	c := colorDepth(os.Stdout)
	writeHeader(os.Stdout, c, []seg{plain("list")}, nil)
	var rels map[string]release
	api, err := newHubClient()
	if err == nil {
		rels, err = hubReleases(api)
	}
	if err != nil {
		// Why there's nothing to compare with, in status's words: the error itself when it tells more.
		why := statusReport{HubStatus: hubFailed, HubMessage: err.Error()}
		var none errNotSignedIn
		var refused errSignInRefused
		var away errUnreachable
		switch {
		case errors.As(err, &none):
			why.HubStatus = hubSignedOut
		case errors.As(err, &refused):
			why.HubStatus = hubRefused
		case errors.As(err, &away):
			why.HubStatus = hubUnreachable
		}
		note, cause := hubNote(why)
		fmt.Println()
		printLines(paragraph(2, plain(note)))
		if cause != "" {
			printLines(paragraph(2, seg{cause, c.dim}))
		}
	}
	// An agent: its name and where its plugins are, then a row each.
	section := func(target string, where []seg, rows []string) {
		fmt.Println()
		head := new(row).at(2, seg{agentName(target), c.bold})
		if len(where) > 0 {
			head.add(plain("  ")).add(where...)
		}
		fmt.Println(head.String())
		printLines(rows)
	}
	// Each plugin: its name, its version, and how it stands against the hub.
	show := func(target string, where []seg, installed map[string]installedPlugin) {
		names, versions := 0, 0
		for name, p := range installed {
			names, versions = max(names, cols(name)), max(versions, cols(clean(p.version)))
		}
		var rows []string
		if len(installed) == 0 {
			rows = []string{new(row).at(5, seg{"nothing from line-lx yet", c.dim}).String()}
		}
		for _, name := range sortedKeys(installed) {
			p := installed[name]
			state := hubState(hub, rels, name, p)
			paint := c.dim
			switch {
			case state == stateCurrent:
				paint = c.green
			case strings.HasPrefix(state, "update available"):
				paint = c.bold
			}
			r := new(row).at(5, seg{name, c.bold}).at(5+names+2, seg{clean(p.version), c.dim})
			if state != "" { // without the hub's answer, there's nothing to compare with
				r.at(5+names+2+versions+3, seg{clean(state), paint})
			}
			rows = append(rows, r.String())
		}
		section(target, where, rows)
	}

	found := false
	if claudeDetected() {
		found = true
		market, _, _, err := claudeConnection(hub)
		note := func(text string) []string { return paragraph(5, seg{text, c.dim}) }
		switch {
		case err != nil:
			section(claudeTarget, nil, note("can't read its settings: "+err.Error()))
		case market == "":
			section(claudeTarget, nil, note("not connected to the hub: run `line-lx sync`"))
		case !claudeOnPath():
			section(claudeTarget, []seg{{market, c.dim}}, note("connected; line-lx lists its plugins only with `claude` on PATH"))
		default:
			// What Claude Code installed from the hub's marketplace, but the bundle: it's not a plugin of the hub.
			versions, err := claudeInstalled(market)
			if err != nil {
				section(claudeTarget, []seg{{market, c.dim}}, note("can't list its plugins: "+err.Error()))
				break
			}
			installed := map[string]installedPlugin{}
			for name, v := range versions {
				if name != bundlePlugin {
					installed[name] = installedPlugin{version: v, hub: hub}
				}
			}
			show(claudeTarget, []seg{{market + " · installed by Claude Code", c.dim}}, installed)
		}
	}
	for _, t := range fileTargets() {
		if t.detected() {
			found = true
			show(t.name, []seg{{tilde(t.dir), c.dim}}, fileInstalled(t))
		}
	}
	if !found {
		fmt.Println()
		printLines(paragraph(2, plain(errNoAgent.Error())))
	}
	return nil
}

// doctorCheck is one of doctor's rows: its outcome, what it checks, what it found (text, then more in dim), what to do
// about it, and a dim note.
type doctorCheck struct {
	kind              int
	label, text, more string
	fix, note         string
}

func runDoctor() error {
	hub, err := currentHub()
	if err != nil {
		return err
	}
	checks := doctorChecks(hub)
	renderDoctor(os.Stdout, checks, colorDepth(os.Stdout))
	for _, k := range checks {
		if k.kind == checkFail {
			return errSilent{}
		}
	}
	return nil
}

// doctorChecks checks the hub, the sign-in and each agent. A failure makes doctor exit 1; a warning doesn't.
func doctorChecks(hub string) []doctorCheck {
	var checks []doctorCheck
	add := func(k doctorCheck) { checks = append(checks, k) }

	res, err := httpClient.Get(hub + "/api/health")
	if err == nil {
		res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK:
		case http.StatusServiceUnavailable: // GET /api/health: the hub answers, Postgres doesn't
			err = errors.New("reachable, but its database is down")
		default:
			err = fmt.Errorf("answered %d", res.StatusCode)
		}
	}
	if err != nil {
		add(doctorCheck{kind: checkFail, label: "Hub", text: hostOf(hub), more: " · " + err.Error()})
	} else {
		add(doctorCheck{kind: checkOK, label: "Hub", text: hostOf(hub), more: " · reachable"})
	}

	var rels map[string]release
	api, err := newHubClient()
	if err == nil {
		var m me
		if err = api.do("GET", "/api/me", nil, &m); err == nil {
			add(doctorCheck{kind: checkOK, label: "Sign-in", text: clean(m.User.Name),
				more: " " + clean(m.User.Email) + " · " + clean(m.Org.Name) + " · " + clean(m.Role)})
			if rels, err = hubReleases(api); err != nil {
				add(doctorCheck{kind: checkFail, label: "Plugins", text: "can't list the hub's plugins: " + err.Error()})
			}
			err = nil
		}
	}
	var none errNotSignedIn
	var refused errSignInRefused
	switch {
	case errors.As(err, &none):
		add(doctorCheck{kind: checkFail, label: "Sign-in", text: none.why, fix: "Run `line-lx login`."})
	case errors.As(err, &refused):
		add(doctorCheck{kind: checkFail, label: "Sign-in", text: "the hub didn't accept it", fix: "Run `line-lx login`."})
	case err != nil:
		add(doctorCheck{kind: checkFail, label: "Sign-in", text: err.Error()})
	}

	found := false
	name := agentName(claudeTarget)
	if !claudeDetected() {
		add(doctorCheck{kind: checkNone, label: name, text: "not found"})
	} else {
		found = true
		settings, _ := claudeSettingsPath()
		market, where, problem, err := claudeConnection(hub)
		if err == nil && market == "" && api != nil {
			err = claudeBlocked(api) // the same checks as sync: a reason to skip isn't a failure
		}
		switch {
		case claudeSkipped(err):
			add(doctorCheck{kind: checkNone, label: name, text: err.Error()})
		case err != nil:
			add(doctorCheck{kind: checkFail, label: name, text: err.Error()})
		case market == "":
			add(doctorCheck{kind: checkFail, label: name, text: "not connected to the hub", more: " · " + tilde(settings), fix: "Run `line-lx sync`."})
		case problem != "" && where != settings:
			add(doctorCheck{kind: checkFail, label: name, text: problem, more: " · " + tilde(where), fix: "Ask who manages Claude Code's managed settings."})
		case problem != "":
			add(doctorCheck{kind: checkFail, label: name, text: problem, more: " · " + tilde(where), fix: "Run `line-lx sync`."})
		default:
			add(doctorCheck{kind: checkOK, label: name, text: "connected", more: " · " + tilde(where)})
		}
		// A warning, not a failure: line-lx can't tell the Claude Code of the desktop app or an IDE.
		if claudeOnPath() {
			out, err := claude("--version")
			version, good := claudeVersionOK(string(out))
			switch {
			case err != nil:
				add(doctorCheck{kind: checkWarn, label: name, text: "can't tell its version: " + err.Error()})
			case !good:
				add(doctorCheck{kind: checkWarn, label: name, text: outdatedText(version), fix: claudeUpdate})
			default:
				add(doctorCheck{kind: checkOK, label: name, text: version, more: " · " + minClaudeVersion + " or later"})
			}
		}
		// A warning, as in the session notice: plugins out for a day that Claude Code still lacks.
		if rels != nil && market != "" {
			if hv := askInstalled(api); hv.status == hubOK && hv.workspace != nil {
				slug := hv.workspace.Slug
				if missing, behind := noticeLag(hub, slug, hv, readClaudeLocal(hub, slug), turnedOff(market)); len(missing) > 0 {
					add(doctorCheck{kind: checkWarn, label: name, text: plural(len(missing), "plugin") + " not installed for over a day", more: " · " + names(missing), fix: "Run `line-lx sync`."})
				} else if len(behind) > 0 {
					add(doctorCheck{kind: checkWarn, label: name, text: plural(len(behind), "plugin") + " behind for over a day", more: " · " + names(behind),
						fix: "Send a message in Claude Code: it updates within 10 minutes. Then run `/reload-plugins`."})
				}
			}
		}
		// A warning: the next report that reaches the hub clears it.
		if msg := droppedText(readDropped()); msg != "" {
			text, fix, _ := strings.Cut(msg, ". ")
			add(doctorCheck{kind: checkWarn, label: "Reports", text: text, fix: fix + "."})
		}
	}

	// compare says how a target's plugins stand against the hub's latest versions.
	compare := func(installed map[string]installedPlugin) (bool, string) {
		if rels == nil {
			return true, plural(len(installed), "plugin") + " installed"
		}
		var current int
		for name := range rels {
			if p, has := installed[name]; has && hubState(hub, rels, name, p) == stateCurrent {
				current++
			}
		}
		return current == len(rels), fmt.Sprintf("%d of %s up to date", current, plural(len(rels), "plugin"))
	}
	for _, t := range fileTargets() {
		if !t.detected() {
			add(doctorCheck{kind: checkNone, label: agentName(t.name), text: "not found"})
			continue
		}
		found = true
		good, msg := compare(fileInstalled(t))
		k := doctorCheck{kind: checkOK, label: agentName(t.name), text: tilde(t.dir), more: " · " + msg, note: t.restart}
		if !good {
			k.kind, k.fix = checkFail, "Run `line-lx sync`."
		}
		add(k)
	}
	if !found {
		add(doctorCheck{kind: checkFail, label: "Agents", text: "none found", fix: "Install Codex, pi or Claude Code."})
	}
	return checks
}

// renderDoctor writes doctor's checks, a row each with its fix under it, then how many problems and warnings.
func renderDoctor(w io.Writer, checks []doctorCheck, c painter) {
	writeHeader(w, c, []seg{plain("doctor")}, nil)
	fmt.Fprintln(w)
	var problems, warnings int
	for _, k := range checks {
		text := plain(k.text)
		if k.kind == checkNone {
			text.paint = c.dim
		}
		rows := checkRow(c, k.kind, k.label, text, seg{k.more, c.dim})
		if k.fix != "" {
			rows = append(rows, fixRow(c, checkCol, k.fix)...)
		}
		if k.note != "" {
			rows = append(rows, noteRow(c, checkCol, k.note)...)
		}
		for _, r := range rows {
			fmt.Fprintln(w, r)
		}
		switch k.kind {
		case checkFail:
			problems++
		case checkWarn:
			warnings++
		}
	}
	fmt.Fprintln(w)
	var sum []seg
	if problems > 0 {
		sum = append(sum, seg{plural(problems, "problem"), c.red})
	}
	if warnings > 0 {
		if len(sum) > 0 {
			sum = append(sum, seg{" · ", c.dim})
		}
		sum = append(sum, seg{plural(warnings, "warning"), c.bold})
	}
	if len(sum) == 0 {
		sum = []seg{{"All good.", c.green}}
	}
	fmt.Fprintln(w, new(row).at(2, sum...).String())
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
