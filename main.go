// Command line-lx puts a company's agent plugins on a developer's machine.
//
// It asks the Metro-LX hub what this person should have, fetches it and writes it for Codex
// and pi, and connects Claude Code to the hub, which then gets it by itself. It moves files;
// the hub makes every decision: it never opens a SKILL.md, translates formats or decides permissions.
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

	// The hub's hook runs it after each use: whatever its flags, arguments or input, it exits 0.
	var agentFlag, workspaceFlag string
	reportUsage := &cobra.Command{
		Use:                "report-usage",
		Short:              "Report one use of your org's plugins to the hub: what the hub's Claude Code hook runs",
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReportUsage(agentFlag, workspaceFlag, cmd.InOrStdin())
		},
	}
	reportUsage.Flags().StringVar(&agentFlag, "agent", "", "the agent that ran it: claude-code")
	reportUsage.Flags().StringVar(&workspaceFlag, "workspace", "", "the workspace's slug")

	// The hub's session hook runs it when a session starts: it prints one JSON object or nothing, and exits 0.
	var noticeAgent, noticeWorkspace string
	var noticeRefresh bool
	noticeCmd := &cobra.Command{
		Use:                "notice",
		Short:              "Say in Claude Code when your plugins have a problem, or were updated: what the hub's session hook runs",
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNotice(noticeAgent, noticeWorkspace, noticeRefresh, cmd.OutOrStdout())
		},
	}
	noticeCmd.Flags().StringVar(&noticeAgent, "agent", "", "the agent whose session starts: claude-code")
	noticeCmd.Flags().StringVar(&noticeWorkspace, "workspace", "", "the workspace's slug")
	// What the notice starts on its own to refresh an expired sign-in, so the hook never holds a refresh.
	noticeCmd.Flags().BoolVar(&noticeRefresh, "refresh", false, "refresh the sign-in and record the hub's answer for the next session")
	_ = noticeCmd.Flags().MarkHidden("refresh")

	var jsonFlag bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show what each agent here has from the hub, and whether it's behind",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runStatus(jsonFlag) },
	}
	status.Flags().BoolVar(&jsonFlag, "json", false, "print it as JSON, in the format the README documents")

	var monthFlag, usageJSON bool
	usage := &cobra.Command{
		Use:   "usage",
		Short: "Show your own runs of your org's plugins over the last 7 days",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runUsage(monthFlag, usageJSON) },
	}
	usage.Flags().BoolVar(&monthFlag, "month", false, "the last 30 days")
	usage.Flags().BoolVar(&usageJSON, "json", false, "print it as JSON, in the format the README documents")

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
		status,
		usage,
		&cobra.Command{Use: "doctor", Short: "Check the Metro-LX hub, your sign-in and each agent", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return runDoctor() }},
		&cobra.Command{Use: "auth-header", Short: "Print your sign-in as an HTTP header: what Claude Code runs to sign in to the hub", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error { return runAuthHeader() }},
		reportUsage,
		noticeCmd,
	)

	if err := root.Execute(); err != nil {
		if _, ok := err.(errSilent); !ok {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
