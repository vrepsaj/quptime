// Package cli wires every user-facing command on the qu binary.
//
// The root command is built lazily via NewRootCommand so test code
// can construct a fresh tree per invocation. Each subcommand lives
// in its own file (init.go, serve.go, node.go, …) and is attached
// from NewRootCommand below.
package cli

import "github.com/spf13/cobra"

// NewRootCommand returns the full cobra tree. version is the build
// stamp surfaced via `qu --version`; pass "dev" when unset.
func NewRootCommand(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "qu",
		Short:         "Quorum-based uptime monitor",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	addInitCmd(root)
	addServeCmd(root)
	addNodeCmd(root)
	addCheckCmd(root)
	addAlertCmd(root)
	addTrustCmd(root)
	addStatusCmd(root)
	addTUICmd(root)
	return root
}
