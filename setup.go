package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// runSetup is the one command a person or their agent runs: sign in if needed, sync, then
// say in plain words what each agent got and what to do next. It never reads stdin and needs no terminal,
// and running it again is safe. Its exit code is sync's, except that no agent on the machine isn't a
// failure here: the sign-in worked, and there's just nothing to write to yet.
func runSetup(hubFlag string) error {
	hub, err := flagOrCurrentHub(hubFlag)
	if err != nil {
		return err
	}
	api, m, err := signedIn(hub)
	if err != nil {
		return err
	}
	results, err := syncAll(api)
	if errors.Is(err, errNoAgent) {
		fmt.Print("\nNo agent found on this machine. line-lx connects Claude Code (~/.claude or `claude` on PATH) " +
			"and writes plugins for Codex (~/.codex) and pi (~/.pi/agent): install one, then run `line-lx setup` again.\n")
		if mcp := clean(m.Endpoints.MCP); mcp != "" {
			fmt.Printf("Claude, ChatGPT and other MCP clients connect to the MCP server instead: %s\n", mcp)
		} else {
			fmt.Println("Claude, ChatGPT and other MCP clients connect to the hub's MCP server instead.")
		}
		return nil
	}
	if results == nil {
		return err
	}
	fmt.Print("\n" + setupSummary(hub, results))
	return err
}

// signedIn returns a client for the hub and who the person is there, signing in through the browser only
// when there's no usable sign-in: none in the keychain, an expired one that won't refresh, or one the hub
// refuses.
func signedIn(hub string) (*hubClient, *me, error) {
	token, err := bearer(hub)
	if err == nil {
		api := &hubClient{hub: hub, token: token}
		var m me
		if err = api.do("GET", "/api/me", nil, &m); err == nil {
			if err := saveHub(hub); err != nil {
				return nil, nil, fmt.Errorf("Can't save the hub in the config: %v", err)
			}
			fmt.Printf("Signed in as %s %s · workspace %s · %s\n", clean(m.User.Name), clean(m.User.Email), clean(m.Org.Name), hub)
			return api, &m, nil
		}
	}
	var none errNotSignedIn
	var refused errSignInRefused
	// LINE_LX_TOKEN is CI's: a browser sign-in never replaces it.
	if os.Getenv("LINE_LX_TOKEN") != "" || !errors.As(err, &none) && !errors.As(err, &refused) {
		return nil, nil, err
	}
	m, err := login(hub)
	if err != nil {
		return nil, nil, err
	}
	token, err = keychainToken(hub)
	if err != nil {
		return nil, nil, err
	}
	return &hubClient{hub: hub, token: token}, m, nil
}

// setupSummary is one line per agent: what it has from the hub, and the next step when something changed.
func setupSummary(hub string, results []*agentResult) string {
	home, _ := os.UserHomeDir()
	var b strings.Builder
	fmt.Fprintf(&b, "line-lx is set up for %s:\n", hub)
	for _, r := range results {
		var msg string
		n := r.delivered + r.current
		switch {
		case r.skipped != "":
			msg = "nothing installed: " + r.skipped + "."
		case r.name == claudeTarget:
			// Claude Code gets the plugins by itself: what line-lx did is connecting it, or failing to.
			msg = r.connected
			if r.failed > 0 {
				msg = "not connected: see above, or run `line-lx doctor`."
			} else if r.warning != "" {
				msg += " " + r.warning + "."
			}
			fmt.Fprintf(&b, "  %-12s %s\n", agentName(r.name), msg)
			continue
		default:
			where := r.where
			if home != "" && strings.HasPrefix(where, home+string(os.PathSeparator)) {
				where = "~" + where[len(home):]
			}
			msg = fmt.Sprintf("%s in %s.", plural(n, "plugin"), where)
		}
		switch {
		case r.failed > 0:
			msg += fmt.Sprintf(" %d failed: see above, or run `line-lx doctor`.", r.failed)
		case r.skipped == "" && r.delivered == 0 && r.removed == 0:
			msg += " Nothing new."
		}
		if r.delivered > 0 || r.removed > 0 {
			msg += " " + nextStep(r.name)
		}
		fmt.Fprintf(&b, "  %-12s %s\n", agentName(r.name), msg)
	}
	b.WriteString("Run `line-lx sync` to get new versions, and `line-lx doctor` to check this machine.\n")
	return b.String()
}

func agentName(target string) string {
	switch target {
	case claudeTarget:
		return "Claude Code"
	case "codex":
		return "Codex"
	}
	return target
}

// nextStep is what the person does for an agent to load what line-lx just wrote.
func nextStep(target string) string {
	switch target {
	case "pi":
		return "Next: restart pi."
	case "codex":
		return "Next: start a new Codex session."
	}
	return ""
}
