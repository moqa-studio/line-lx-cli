package main

import (
	"errors"
	"fmt"
	"os"
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
	c := colorDepth(os.Stdout)
	writeHeader(os.Stdout, c, []seg{plain("setup")}, nil)
	fmt.Println()
	api, m, err := signedIn(hub)
	if err != nil {
		return err
	}
	results, err := syncAll(api)
	if errors.Is(err, errNoAgent) {
		fmt.Println()
		printLines(paragraph(2, plain("No agent found on this machine. line-lx connects Claude Code (~/.claude or `claude` on PATH) "+
			"and writes plugins for Codex (~/.codex) and pi (~/.pi/agent): install one, then run `line-lx setup` again.")))
		if mcp := clean(m.Endpoints.MCP); mcp != "" {
			printLines(paragraph(2, seg{"Claude, ChatGPT and other MCP clients connect to the MCP server instead: " + mcp, c.dim}))
		} else {
			printLines(paragraph(2, seg{"Claude, ChatGPT and other MCP clients connect to the hub's MCP server instead.", c.dim}))
		}
		return nil
	}
	if results == nil {
		return err
	}
	printLines(paragraph(2, seg{"Run `line-lx sync` for new versions, and `line-lx doctor` to check this machine.", c.dim}))
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
			printLines(signInRow(&m, hub, colorDepth(os.Stdout)))
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

// agentName is an agent's name for people, as the web app writes it; an agent line-lx doesn't know keeps its id.
func agentName(target string) string {
	switch target {
	case claudeTarget:
		return "Claude Code"
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	case "mcp":
		return "MCP server"
	}
	return target
}

// nextStep is what the person does for an agent to load what line-lx just wrote.
func nextStep(target string) string {
	switch target {
	case "pi":
		return "Restart pi to load them."
	case "codex":
		return "Start a new Codex session to load them."
	}
	return ""
}
