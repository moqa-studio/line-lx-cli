// Command line-lx puts a company's agent plugins on a developer's machine.
//
// It asks the Metro-LX hub what this person should have, fetches it and writes it for each
// local agent (Codex, pi, Claude Code). It moves files; the hub makes every decision:
// it never opens a SKILL.md, translates formats or decides permissions.
//
// Not built yet: `line-lx add` / `line-lx remove`. The hub has no endpoint for personal
// choices, so everyone gets all of their org's published plugins.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is set at release time with -ldflags "-X main.version=…".
var version = "dev"

// errSilent marks a failure whose details were already printed: exit 1, say nothing more.
type errSilent struct{}

func (errSilent) Error() string { return "" }

func main() {
	root := &cobra.Command{
		Use:           "line-lx",
		Short:         "Put your company's agent plugins on this machine",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	var hubFlag string
	login := &cobra.Command{
		Use:   "login",
		Short: "Sign in to the Metro-LX hub in your browser",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runLogin(hubFlag) },
	}
	login.Flags().StringVar(&hubFlag, "hub", "", "the Metro-LX hub's URL (remembered for the next commands)")

	setup := &cobra.Command{
		Use:   "setup",
		Short: "Sign in if needed, then put your org's plugins in every agent on this machine",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runSetup(hubFlag) },
	}
	setup.Flags().StringVar(&hubFlag, "hub", "", "the Metro-LX hub's URL (remembered for the next commands)")

	root.AddCommand(
		setup,
		login,
		&cobra.Command{Use: "logout", Short: "Forget this machine's sign-in to the Metro-LX hub", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return runLogout() }},
		&cobra.Command{Use: "whoami", Short: "Show who you are signed in as", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return runWhoami() }},
		&cobra.Command{Use: "sync", Short: "Write your org's plugins to every agent on this machine", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return runSync() }},
		&cobra.Command{Use: "list", Short: "List the plugins installed here, per agent", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return runList() }},
		&cobra.Command{Use: "doctor", Short: "Check the Metro-LX hub, your sign-in and each agent", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return runDoctor() }},
	)

	if err := root.Execute(); err != nil {
		if _, ok := err.(errSilent); !ok {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
