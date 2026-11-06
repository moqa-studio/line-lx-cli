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

var errNoAgent = errors.New("No agent found on this machine (looked for ~/.codex, ~/.pi/agent and `claude` on PATH).")

// agentResult is what a sync did for one agent, for `line-lx setup`'s summary.
type agentResult struct {
	name, where                string
	delivered, current, failed int
	removed                    int    // plugins line-lx took out of the agent
	synced                     int    // Claude Code: plugins it already has from the person's claude.ai account
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

// syncAll writes the hub's plugins to every agent found here and reports each delivery to the hub. It
// returns errNoAgent when there's none, and errSilent when a target failed (the details are printed).
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
	var written, installed, upToDate, synced, failed, reported int
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
		case target == claudeTarget: // Claude Code installs; it never reports skipped files
			msg = "installed"
			installed++
			res.delivered++
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

	if _, ok := claudeDetected(); ok {
		res := &agentResult{name: claudeTarget}
		results = append(results, res)
		market, err := claudePrepare(api)
		var have, fromAccount map[string]string
		if err == nil {
			res.where = market.name
			have, fromAccount, err = claudeInstalled(market.name)
		}
		var clash errMarketplaceClash
		switch {
		case errors.Is(err, errNoMarketplace), errors.As(err, &clash):
			// Nothing line-lx can install: one line, nothing reported.
			res.skipped = err.Error()
			fmt.Printf("%s skipped: %v.\n", claudeTarget, err)
		case err != nil:
			if len(rels) == 0 { // no plugin to report it with, but it still shows and fails the sync
				line("", claudeTarget, "failed: "+err.Error())
				failed++
				res.failed++
			}
			for _, r := range rels {
				deliver(res, r, nil, err)
			}
		default:
			for _, r := range rels {
				v, ok := have[r.name]
				id := r.name + "@" + market.name
				_, isSynced := fromAccount[r.name]
				switch {
				case !validName(r.name):
					deliver(res, r, nil, fmt.Errorf("invalid plugin name %q", clean(r.name)))
				case isSynced:
					// The Claude connector already puts it in Claude Code, through the person's claude.ai
					// account. A second copy from line-lx would load each skill twice, so line-lx installs none,
					// and removes the one it installed before. Nothing is reported: a claude-code delivery
					// says line-lx installed this version on this machine, which it didn't. The hub already
					// has the claude delivery for it, and Claude Code follows that one.
					msg := "already in Claude Code through your claude.ai account"
					if ok {
						if _, err := claude("plugin", "uninstall", "--", id); err != nil {
							msg = "already in Claude Code through your claude.ai account, but line-lx's copy stays: " + err.Error()
							failed++
							res.failed++
						} else {
							msg += ": removed line-lx's copy"
							res.removed++
						}
					}
					line(label(r.name, r.version.Version), claudeTarget, msg)
					synced++
					res.synced++
				case market.failed[r.name] != nil:
					deliver(res, r, nil, market.failed[r.name])
				case !market.listed[r.name]:
					deliver(res, r, nil, errors.New("not in the hub's Claude Code marketplace"))
				case ok && v == r.version.Version:
					current(res, r)
				case ok:
					_, cerr := claude("plugin", "update", "--", id)
					deliver(res, r, nil, cerr)
				default:
					_, cerr := claude("plugin", "install", "--", id)
					deliver(res, r, nil, cerr)
				}
			}
			// Plugins this hub no longer has, from its marketplace only. A plugin still installed keeps
			// its folder, which Claude Code reads it from.
			keep := market.listed
			for _, name := range sortedKeys(have) {
				if inHub[name] {
					continue
				}
				if _, err := claude("plugin", "uninstall", "--", name+"@"+market.name); err != nil {
					line(label(name, have[name]), claudeTarget, "failed to remove: "+err.Error())
					failed++
					res.failed++
					keep[name] = true
				} else {
					line(label(name, have[name]), claudeTarget, "removed (not in the hub anymore)")
					res.removed++
				}
			}
			if err := market.prune(keep); err != nil {
				line("", claudeTarget, "failed to clean up "+market.dir+": "+err.Error())
				failed++
				res.failed++
			}
		}
	}

	if len(results) == 0 {
		return nil, errNoAgent
	}

	summary := fmt.Sprintf("%d written, %d installed, %d up to date, ", written, installed, upToDate)
	if synced > 0 {
		summary += fmt.Sprintf("%d already in Claude Code through claude.ai, ", synced)
	}
	summary += fmt.Sprintf("%d failed.", failed)
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
	synced, twice           bool // Claude Code: it comes from the person's claude.ai account, and from line-lx too
}

const (
	stateCurrent = "up to date"
	stateSynced  = "through your claude.ai account (@synced)"
)

// hubState compares an installed plugin with the hub's latest. rels is nil when the hub didn't answer.
func hubState(hub string, rels map[string]release, name string, p installedPlugin) string {
	switch {
	case p.twice:
		return stateSynced + " and from line-lx: run `line-lx sync`"
	case p.synced:
		return stateSynced
	case p.hub != hub:
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

// claudeInstalledPlugins lists what Claude Code has from this hub's marketplace (empty when not added),
// and the hub's plugins (in rels) it has synced from the person's claude.ai account.
func claudeInstalledPlugins(hub string, rels map[string]release) (map[string]installedPlugin, string, error) {
	market, err := claudeMarketplace(hub)
	if err != nil || market == "" {
		return nil, market, err
	}
	installed, synced, err := claudeInstalled(market)
	out := map[string]installedPlugin{}
	for name, v := range installed {
		out[name] = installedPlugin{version: v, hub: hub}
	}
	for name, v := range synced {
		if _, inHub := rels[name]; inHub {
			_, twice := installed[name]
			out[name] = installedPlugin{version: v, hub: hub, synced: true, twice: twice}
		}
	}
	return out, market, err
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
	if _, ok := claudeDetected(); ok {
		found = true
		dir, _ := claudeDir(hub)
		installed, market, err := claudeInstalledPlugins(hub, rels)
		title := fmt.Sprintf("%s (%s)", claudeTarget, dir)
		if market != "" {
			title = fmt.Sprintf("%s (%s, %s)", claudeTarget, market, dir)
		}
		if err != nil {
			fmt.Printf("%s\n  can't list its plugins: %v\n", title, err)
		} else {
			show(title, installed)
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
			// A plugin synced from the claude.ai account follows the hub through the Claude connector.
			if p, has := installed[name]; has && (hubState(hub, rels, name, p) == stateCurrent || p.synced && !p.twice) {
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
	if _, detected := claudeDetected(); !detected {
		fmt.Printf("     %-12s not found (no `claude` on PATH)\n", claudeTarget)
	} else {
		found = true
		dir, _ := claudeDir(hub)
		installed, market, err := claudeInstalledPlugins(hub, rels)
		if err == nil && market == "" && api != nil {
			err = claudeBlocked(api) // the same checks as sync: a reason to skip isn't a failure
		}
		var clash errMarketplaceClash
		skipped := errors.Is(err, errNoMarketplace) || errors.As(err, &clash)
		switch {
		case skipped:
			check(claudeTarget, true, err.Error())
		case err != nil:
			check(claudeTarget, false, dir+" · "+err.Error())
		case market == "" && len(rels) > 0:
			check(claudeTarget, false, dir+" · the hub's marketplace isn't added: run `line-lx sync`")
		default:
			good, msg := compare(installed)
			check(claudeTarget, good, dir+" · "+msg)
		}
		if !skipped {
			fmt.Printf("     %-12s %s\n", "", claudeRestart)
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
