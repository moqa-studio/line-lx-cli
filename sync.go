package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

const claudeTarget = "claude-code"

var errNoAgent = errors.New("No agent found on this machine (looked for ~/.codex, ~/.pi/agent, and ~/.claude or `claude` on PATH).")

// agentResult is what a sync did for one agent, for `line-lx setup`'s summary.
type agentResult struct {
	name, where                string
	delivered, current, failed int
	removed                    int    // plugins line-lx took out of the agent
	connected                  string // Claude Code: what connecting it did
	skipped                    string // why the agent got nothing, when sync skipped it
}

func runSync() error {
	api, err := newHubClient()
	if err != nil {
		return err
	}
	_, err = syncAll(api)
	return err
}

// syncAll writes the hub's plugins to every agent found here and reports each delivery to the hub, and
// connects Claude Code to the hub, which then gets the plugins by itself. It returns errNoAgent when
// there's no agent, and errSilent when a target failed (the details are printed).
func syncAll(api *hubClient) ([]*agentResult, error) {
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
		fmt.Println("Your workspace has no published plugins yet.")
	}

	label := func(name, version string) string { return clean(name + " " + version) }
	width := 0
	for _, r := range rels {
		width = max(width, len(label(r.name, r.version.Version)))
	}
	for _, t := range fileTargets() { // plugins about to be removed are printed too
		for name, s := range t.stamps() {
			width = max(width, len(label(name, s.Version)))
		}
	}
	line := func(what, target, msg string) { fmt.Printf("%-*s  → %-12s %s\n", width, what, target, msg) }
	home, _ := os.UserHomeDir()

	var results []*agentResult
	var written, upToDate, failed, reported int
	var reportErr error
	// deliver prints the outcome of one plugin × target and reports it to the hub.
	deliver := func(res *agentResult, r release, unsupported []string, werr error) {
		target := res.name
		status, msg, errMsg := "delivered", "written", ""
		switch {
		case werr != nil:
			// The hub gets paths relative to the home folder, not this machine's layout.
			errMsg = werr.Error()
			if home != "" {
				errMsg = strings.ReplaceAll(errMsg, home, "~")
			}
			status, msg = "failed", "failed: "+werr.Error()
			failed++
			res.failed++
		case len(unsupported) > 0:
			status, msg = "partial", "written, skipped: "+strings.Join(unsupported, "; ")
			written++
			res.delivered++
		default:
			written++
			res.delivered++
		}
		line(label(r.name, r.version.Version), target, msg)
		if err := api.report(r.version.ID, target, status, unsupported, errMsg); err != nil {
			reportErr = err
		} else {
			reported++
		}
	}
	current := func(res *agentResult, r release) {
		line(label(r.name, r.version.Version), res.name, "up to date")
		upToDate++
		res.current++
	}

	inHub := map[string]bool{}
	for _, r := range rels {
		inHub[r.name] = true
	}

	for _, t := range fileTargets() {
		if !t.detected() {
			continue
		}
		res := &agentResult{name: t.name, where: t.dir}
		results = append(results, res)
		if err := t.sweepStaging(); err != nil { // leftovers of an interrupted sync
			line("", t.name, "failed: "+err.Error())
			failed++
			res.failed++
			continue
		}
		stamps := t.stamps()
		for _, r := range rels {
			if s, ok := stamps[r.name]; ok && s.Hub == api.hub && s.VersionID == r.version.ID {
				current(res, r)
				continue
			}
			unsupported, err := t.write(api.hub, r)
			deliver(res, r, unsupported, err)
		}
		// Plugins this hub no longer has: only folders carrying our stamp, and only this hub's.
		for _, name := range sortedKeys(stamps) {
			s := stamps[name]
			if s.Hub != api.hub || inHub[name] {
				continue
			}
			if err := t.remove(name, s); err != nil {
				line(label(name, s.Version), t.name, "failed to remove: "+err.Error())
				failed++
				res.failed++
			} else {
				line(label(name, s.Version), t.name, "removed (not in the hub anymore)")
				res.removed++
			}
		}
		_ = t.sweepStaging() // nothing left behind once this target is done
	}

	// Claude Code: line-lx writes no plugin, it connects Claude Code to the hub. Nothing is reported to the
	// hub: Claude Code fetches the plugins itself.
	if claudeDetected() {
		res := &agentResult{name: claudeTarget}
		results = append(results, res)
		msg, err := claudeConnect(api)
		switch {
		case claudeSkipped(err):
			res.skipped = err.Error()
			fmt.Printf("%-12s skipped: %v.\n", claudeTarget, err)
		case err != nil:
			fmt.Printf("%-12s failed: %v\n", claudeTarget, err)
			failed++
			res.failed++
		default:
			res.connected = msg
			fmt.Printf("%-12s %s\n", claudeTarget, msg)
		}
	}

	if len(results) == 0 {
		return nil, errNoAgent
	}

	summary := fmt.Sprintf("%d written, %d up to date, %d failed.", written, upToDate, failed)
	switch {
	case reportErr != nil:
		summary += " Couldn't report every delivery to the hub: " + reportErr.Error()
	case reported > 0:
		summary += " Reported to the hub."
	}
	fmt.Println(summary)
	if failed > 0 || reportErr != nil {
		return results, errSilent{}
	}
	return results, nil
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
	var rels map[string]release
	api, err := newHubClient()
	if err == nil {
		rels, err = hubReleases(api)
	}
	if err != nil {
		fmt.Printf("The hub didn't answer, so this is only what's installed. (%v)\n", err)
	}
	show := func(title string, installed map[string]installedPlugin) {
		fmt.Println(title)
		if len(installed) == 0 {
			fmt.Println("  nothing from line-lx yet")
		}
		for _, name := range sortedKeys(installed) {
			p := installed[name]
			fmt.Printf("  %-24s %-10s %s\n", name, clean(p.version), hubState(hub, rels, name, p))
		}
	}

	found := false
	for _, t := range fileTargets() {
		if t.detected() {
			found = true
			show(fmt.Sprintf("%s (%s)", t.name, t.dir), fileInstalled(t))
		}
	}
	if claudeDetected() {
		found = true
		market, _, _, err := claudeConnection(hub)
		switch {
		case err != nil:
			fmt.Printf("%s\n  can't read its settings: %v\n", claudeTarget, err)
		case market == "":
			fmt.Printf("%s\n  not connected to the hub: run `line-lx sync`\n", claudeTarget)
		case !claudeOnPath():
			fmt.Printf("%s (%s)\n  connected; line-lx lists its plugins only with `claude` on PATH\n", claudeTarget, market)
		default:
			// What Claude Code installed from the hub's marketplace, but the bundle: it's not a plugin of the hub.
			versions, err := claudeInstalled(market)
			if err != nil {
				fmt.Printf("%s (%s)\n  can't list its plugins: %v\n", claudeTarget, market, err)
				break
			}
			installed := map[string]installedPlugin{}
			for name, v := range versions {
				if name != bundlePlugin {
					installed[name] = installedPlugin{version: v, hub: hub}
				}
			}
			show(fmt.Sprintf("%s (%s, installed by Claude Code)", claudeTarget, market), installed)
		}
	}
	if !found {
		fmt.Println(errNoAgent)
	}
	return nil
}

func runDoctor() error {
	hub, err := currentHub()
	if err != nil {
		return err
	}
	ok := true
	check := func(name string, good bool, msg string) {
		mark := "ok  "
		if !good {
			mark, ok = "FAIL", false
		}
		fmt.Printf("%s %-12s %s\n", mark, name, msg)
	}

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
		check("hub", false, hub+" · "+err.Error())
	} else {
		check("hub", true, hub+" · reachable")
	}

	var rels map[string]release
	api, err := newHubClient()
	if err == nil {
		var m me
		if err = api.do("GET", "/api/me", nil, &m); err == nil {
			check("sign-in", true, fmt.Sprintf("%s %s · workspace %s (%s)", clean(m.User.Name), clean(m.User.Email), clean(m.Org.Name), clean(m.Role)))
			rels, err = hubReleases(api)
		}
	}
	if err != nil {
		check("sign-in", false, err.Error())
	}

	// compare says how a target's plugins stand against the hub's latest versions.
	compare := func(installed map[string]installedPlugin) (bool, string) {
		if rels == nil {
			return true, plural(len(installed), "plugin") + " installed"
		}
		var current, behind int
		for name := range rels {
			if p, has := installed[name]; has && hubState(hub, rels, name, p) == stateCurrent {
				current++
			} else {
				behind++
			}
		}
		msg := fmt.Sprintf("%d of %s up to date", current, plural(len(rels), "plugin"))
		if behind > 0 {
			msg += ": run `line-lx sync`"
		}
		return behind == 0, msg
	}

	found := false
	for _, t := range fileTargets() {
		if !t.detected() {
			fmt.Printf("     %-12s not found (no %s)\n", t.name, t.home)
			continue
		}
		found = true
		good, msg := compare(fileInstalled(t))
		check(t.name, good, t.dir+" · "+msg)
		if t.restart != "" {
			fmt.Printf("     %-12s %s\n", "", t.restart)
		}
	}
	if !claudeDetected() {
		fmt.Printf("     %-12s not found (no ~/.claude, no `claude` on PATH)\n", claudeTarget)
	} else {
		found = true
		settings, _ := claudeSettingsPath()
		market, where, problem, err := claudeConnection(hub)
		if err == nil && market == "" && api != nil {
			err = claudeBlocked(api) // the same checks as sync: a reason to skip isn't a failure
		}
		switch {
		case claudeSkipped(err):
			check(claudeTarget, true, err.Error())
		case err != nil:
			check(claudeTarget, false, err.Error())
		case market == "":
			check(claudeTarget, false, settings+" · not connected to the hub: run `line-lx sync`")
		case problem != "" && where != settings:
			check(claudeTarget, false, where+" · "+problem+": ask who manages Claude Code's managed settings")
		case problem != "":
			check(claudeTarget, false, where+" · "+problem+": run `line-lx sync`")
		default:
			check(claudeTarget, true, where+" · connected: Claude Code gets the hub's plugins as "+bundlePlugin+"@"+market)
		}
		// A warning, not a failure: line-lx can't tell the Claude Code of the desktop app or an IDE.
		if claudeOnPath() {
			out, err := claude("--version")
			version, good := claudeVersionOK(string(out))
			switch {
			case err != nil:
				fmt.Printf("warn %-12s can't tell Claude Code's version: %v\n", "", err)
			case !good:
				fmt.Printf("warn %-12s Claude Code %s can't sign in to the hub: update it to %s or later\n", "", version, minClaudeVersion)
			default:
				fmt.Printf("     %-12s Claude Code %s\n", "", version)
			}
		}
	}
	if !found {
		check("agents", false, "none found: install Codex, pi or Claude Code")
	}

	if !ok {
		return errSilent{}
	}
	return nil
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
