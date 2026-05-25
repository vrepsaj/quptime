package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

// addClusterCmd registers the `qu cluster` parent and its subcommands.
// Today this is just the resolver-list editor; future cluster-wide
// settings (rate limits, default intervals, etc.) would live here too.
func addClusterCmd(root *cobra.Command) {
	cluster := &cobra.Command{
		Use:   "cluster",
		Short: "Manage cluster-wide settings (currently: default DNS resolvers)",
	}

	resolvers := &cobra.Command{
		Use:   "resolvers",
		Short: "View or edit the cluster-wide default DNS resolver list",
		Long: `The cluster-wide resolver list is used by any check that has no
per-check resolvers set. Useful when every check should bypass the
host's stub resolver in the same way — e.g. point the whole cluster
at Cloudflare's 1.1.1.1 / 1.0.0.1 to avoid stale local caches.

Per-check resolvers (set with "qu check add --resolvers …" or
"qu check edit --resolvers …") always win over the cluster default.`,
	}

	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Print the current cluster-wide resolver list",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadClusterConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(cfg.Resolvers) == 0 {
				fmt.Fprintln(out, "(none — checks fall back to each host's system resolver)")
				return nil
			}
			for _, r := range cfg.Resolvers {
				fmt.Fprintln(out, r)
			}
			return nil
		},
	}

	setCmd := &cobra.Command{
		Use:   "set <resolver1> [<resolver2> ...]",
		Short: "Replace the cluster-wide resolver list (tried in order, with connection-level failover)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			cleaned := make([]string, 0, len(args))
			for _, a := range args {
				for _, p := range strings.Split(a, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						cleaned = append(cleaned, p)
					}
				}
			}
			if len(cleaned) == 0 {
				return fmt.Errorf("no resolvers given; pass at least one host[:port]")
			}
			payload, err := json.Marshal(cleaned)
			if err != nil {
				return err
			}
			body := daemon.MutateBody{Kind: transport.MutationSetResolvers, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "cluster resolvers set to %s (cluster version now %d)\n",
				strings.Join(cleaned, ", "), res.Version)
			return nil
		},
	}

	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove the cluster-wide resolver list (every check then falls back to its host's system resolver)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			payload, _ := json.Marshal([]string{})
			body := daemon.MutateBody{Kind: transport.MutationSetResolvers, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "cluster resolvers cleared (cluster version now %d)\n", res.Version)
			return nil
		},
	}

	resolvers.AddCommand(showCmd, setCmd, clearCmd)
	cluster.AddCommand(resolvers)
	root.AddCommand(cluster)
}
