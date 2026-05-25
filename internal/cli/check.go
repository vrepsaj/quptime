package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

func addCheckCmd(root *cobra.Command) {
	check := &cobra.Command{
		Use:   "check",
		Short: "Manage configured checks",
	}

	addHTTP := buildAddCheckCmd(config.CheckHTTP, "http", "<name> <url>",
		"Add an HTTP/HTTPS check",
		func(args []string, c *config.Check) error {
			c.Name = args[0]
			c.Target = args[1]
			return nil
		})
	addHTTP.Flags().Int("expect", 200, "HTTP status code that signals UP")
	addHTTP.Flags().String("body-match", "", "substring required in response body for UP")

	addTCP := buildAddCheckCmd(config.CheckTCP, "tcp", "<name> <host:port>",
		"Add a TCP-connect check",
		func(args []string, c *config.Check) error {
			c.Name = args[0]
			c.Target = args[1]
			return nil
		})

	addICMP := buildAddCheckCmd(config.CheckICMP, "icmp", "<name> <host>",
		"Add an ICMP ping check",
		func(args []string, c *config.Check) error {
			c.Name = args[0]
			c.Target = args[1]
			return nil
		})

	addTLS := buildAddCheckCmd(config.CheckTLS, "tls", "<name> <host[:port]>",
		"Add a TLS cert-expiry check",
		func(args []string, c *config.Check) error {
			c.Name = args[0]
			c.Target = args[1]
			return nil
		})
	addTLS.Flags().Int("warn-days", 14, "fail the check when the cert has fewer than this many days of validity left")
	addTLS.Flags().String("sni", "", "override the SNI sent during the handshake (default: host from target)")

	addDNS := buildAddCheckCmd(config.CheckDNS, "dns", "<name> <hostname>",
		"Add a DNS resolution check",
		func(args []string, c *config.Check) error {
			c.Name = args[0]
			c.Target = args[1]
			return nil
		})
	addDNS.Flags().String("record", "a", "DNS record type to query: a|aaaa|cname|mx|txt|ns")
	addDNS.Flags().String("resolver", "", "resolver to query, e.g. 1.1.1.1:53 (default: system resolver)")
	addDNS.Flags().String("expect", "", "substring that must appear in at least one answer (optional)")

	addParent := &cobra.Command{
		Use:   "add",
		Short: "Add a new check",
	}
	addParent.AddCommand(addHTTP, addTCP, addICMP, addTLS, addDNS)

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List configured checks and their current aggregate state",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			return runStatusPrintChecks(ctx, cmd)
		},
	}

	removeCmd := &cobra.Command{
		Use:   "remove <id-or-name>",
		Short: "Remove a configured check",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			body := daemon.MutateBody{Kind: transport.MutationRemoveCheck}
			payload, _ := json.Marshal(args[0])
			body.Payload = payload
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "removed check %s (cluster version now %d)\n", args[0], res.Version)
			return nil
		},
	}

	testCmd := &cobra.Command{
		Use:   "test <id-or-name>",
		Short: "Fire a synthetic transition through this check's effective alerts",
		Long: `Render and ship a synthetic state-transition message for a real check
through every alert that would actually receive it. Useful for
validating alert templates and channel wiring without waiting for a
real outage. The hysteresis filter that normally suppresses
Unknown→Up transitions is bypassed: --state up will fire.

Default is --state down, the transition most worth exercising.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			state, _ := cmd.Flags().GetString("state")
			body := daemon.CheckTestBody{CheckID: args[0], State: state}
			if _, err := callDaemon(ctx, daemon.CtrlCheckTest, body); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "test %s transition fired for check %s\n",
				normaliseTestState(state), args[0])
			return nil
		},
	}
	testCmd.Flags().String("state", "down", "synthetic transition to render: down|up|recovered")

	enableCmd := buildCheckToggleCmd("enable", false,
		"Re-enable a paused check so the scheduler probes it again")
	disableCmd := buildCheckToggleCmd("disable", true,
		"Pause a check: the scheduler stops probing it and no alerts fire from its state")

	check.AddCommand(addParent, listCmd, removeCmd, testCmd, enableCmd, disableCmd, buildCheckEditCmd())
	root.AddCommand(check)
}

// buildCheckToggleCmd returns the `qu check enable|disable` subcommand.
// Both share an implementation: look up the check, flip Disabled, and
// re-submit it through the standard AddCheck mutation (which replaces
// any existing entry with matching ID).
func buildCheckToggleCmd(use string, disabled bool, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id-or-name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			cluster, err := config.LoadClusterConfig()
			if err != nil {
				return err
			}
			var existing *config.Check
			for i := range cluster.Checks {
				if cluster.Checks[i].ID == args[0] || cluster.Checks[i].Name == args[0] {
					cp := cluster.Checks[i]
					existing = &cp
					break
				}
			}
			if existing == nil {
				return fmt.Errorf("no check named %q", args[0])
			}
			if existing.Disabled == disabled {
				fmt.Fprintf(cmd.OutOrStdout(), "check %s already %sd\n", existing.Name, use)
				return nil
			}
			existing.Disabled = disabled
			payload, err := json.Marshal(existing)
			if err != nil {
				return err
			}
			body := daemon.MutateBody{Kind: transport.MutationAddCheck, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "%sd check %s (cluster version now %d)\n",
				use, existing.Name, res.Version)
			return nil
		},
	}
}

// normaliseTestState mirrors the dispatcher's parsing so the CLI's
// success message matches what was actually fired.
func normaliseTestState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "down":
		return "down"
	case "up":
		return "up"
	case "recovered":
		return "recovered"
	}
	return s
}

// buildCheckEditCmd returns `qu check edit`, which updates fields of an
// existing check in place. Only flags that the operator actually passes
// modify the corresponding field — everything else is preserved from the
// existing record, including the ID. Identity match is by ID or Name.
func buildCheckEditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <id-or-name>",
		Short: "Update fields of an existing check",
		Long: `Update one or more fields of an existing check.

Identifies the target by ID or Name. Only flags you pass take effect;
all other fields are preserved from the existing record. HTTP-only flags
(--expect, --body-match) error out on non-HTTP checks.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()

			cluster, err := config.LoadClusterConfig()
			if err != nil {
				return err
			}
			snap := cluster.Snapshot()
			var existing *config.Check
			for i := range snap.Checks {
				if snap.Checks[i].ID == args[0] || snap.Checks[i].Name == args[0] {
					cp := snap.Checks[i]
					existing = &cp
					break
				}
			}
			if existing == nil {
				return fmt.Errorf("no check named %q", args[0])
			}

			f := cmd.Flags()
			if f.Changed("name") {
				v, _ := f.GetString("name")
				existing.Name = strings.TrimSpace(v)
			}
			if f.Changed("target") {
				v, _ := f.GetString("target")
				existing.Target = strings.TrimSpace(v)
			}
			if f.Changed("interval") {
				s, _ := f.GetString("interval")
				d, err := time.ParseDuration(s)
				if err != nil {
					return fmt.Errorf("--interval: %w", err)
				}
				existing.Interval = d
			}
			if f.Changed("timeout") {
				s, _ := f.GetString("timeout")
				d, err := time.ParseDuration(s)
				if err != nil {
					return fmt.Errorf("--timeout: %w", err)
				}
				existing.Timeout = d
			}
			if f.Changed("alerts") {
				csv, _ := f.GetString("alerts")
				existing.AlertIDs = nil
				for _, p := range strings.Split(csv, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						existing.AlertIDs = append(existing.AlertIDs, p)
					}
				}
			}
			if f.Changed("resolvers") {
				rs, _ := f.GetStringSlice("resolvers")
				existing.Resolvers = nil
				for _, r := range rs {
					r = strings.TrimSpace(r)
					if r != "" {
						existing.Resolvers = append(existing.Resolvers, r)
					}
				}
			}
			if f.Changed("expect") {
				v, _ := f.GetString("expect")
				switch existing.Type {
				case config.CheckHTTP:
					n, err := parsePositiveInt(v)
					if err != nil {
						return fmt.Errorf("--expect: %w", err)
					}
					existing.ExpectStatus = n
				case config.CheckDNS:
					existing.DNSExpect = strings.TrimSpace(v)
				default:
					return fmt.Errorf("--expect only applies to HTTP or DNS checks (this is %s)", existing.Type)
				}
			}
			if f.Changed("body-match") {
				if existing.Type != config.CheckHTTP {
					return fmt.Errorf("--body-match only applies to HTTP checks (this is %s)", existing.Type)
				}
				v, _ := f.GetString("body-match")
				existing.BodyMatch = v
			}
			if f.Changed("warn-days") {
				if existing.Type != config.CheckTLS {
					return fmt.Errorf("--warn-days only applies to TLS checks (this is %s)", existing.Type)
				}
				v, _ := f.GetInt("warn-days")
				existing.TLSWarnDays = v
			}
			if f.Changed("sni") {
				if existing.Type != config.CheckTLS {
					return fmt.Errorf("--sni only applies to TLS checks (this is %s)", existing.Type)
				}
				v, _ := f.GetString("sni")
				existing.TLSServerName = strings.TrimSpace(v)
			}
			if f.Changed("record") {
				if existing.Type != config.CheckDNS {
					return fmt.Errorf("--record only applies to DNS checks (this is %s)", existing.Type)
				}
				v, _ := f.GetString("record")
				existing.DNSRecord = strings.ToLower(strings.TrimSpace(v))
			}
			if f.Changed("resolver") {
				if existing.Type != config.CheckDNS {
					return fmt.Errorf("--resolver only applies to DNS checks (this is %s)", existing.Type)
				}
				v, _ := f.GetString("resolver")
				existing.DNSResolver = strings.TrimSpace(v)
			}

			payload, err := json.Marshal(existing)
			if err != nil {
				return err
			}
			body := daemon.MutateBody{Kind: transport.MutationAddCheck, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "updated check %s (cluster version now %d)\n", existing.Name, res.Version)
			return nil
		},
	}
	cmd.Flags().String("name", "", "rename the check")
	cmd.Flags().String("target", "", "new probe target (URL, host:port, or host)")
	cmd.Flags().String("interval", "", "new probe interval (e.g. 30s, 1m)")
	cmd.Flags().String("timeout", "", "new per-probe timeout (e.g. 10s)")
	cmd.Flags().String("alerts", "", "replace alert list with this CSV of IDs/names (pass empty to clear)")
	cmd.Flags().StringSlice("resolvers", nil, "replace the resolver list (e.g. --resolvers 1.1.1.1,1.0.0.1). Pass --resolvers '' to clear and fall back to the cluster default.")
	cmd.Flags().String("expect", "", "HTTP: expected status code; DNS: substring required in an answer")
	cmd.Flags().String("body-match", "", "substring required in body (HTTP only)")
	cmd.Flags().Int("warn-days", 0, "TLS: fail when cert expires within this many days")
	cmd.Flags().String("sni", "", "TLS: override SNI sent during handshake")
	cmd.Flags().String("record", "", "DNS: record type (a|aaaa|cname|mx|txt|ns)")
	cmd.Flags().String("resolver", "", "DNS: resolver host:port (e.g. 1.1.1.1:53)")
	return cmd
}

// parsePositiveInt is a small wrapper around strconv used to validate
// the HTTP --expect status code in the unified --expect string flag.
func parsePositiveInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a positive integer: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// buildAddCheckCmd produces the per-type "qu check add <type>" subcommand.
func buildAddCheckCmd(ctype config.CheckType, use, argSpec, short string,
	bind func(args []string, c *config.Check) error,
) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use + " " + argSpec,
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			ch := config.Check{
				ID:   uuid.NewString(),
				Type: ctype,
			}
			if err := bind(args, &ch); err != nil {
				return err
			}
			intervalStr, _ := cmd.Flags().GetString("interval")
			timeoutStr, _ := cmd.Flags().GetString("timeout")
			alertsCSV, _ := cmd.Flags().GetString("alerts")
			if intervalStr != "" {
				d, err := time.ParseDuration(intervalStr)
				if err != nil {
					return fmt.Errorf("--interval: %w", err)
				}
				ch.Interval = d
			} else {
				ch.Interval = 30 * time.Second
			}
			if timeoutStr != "" {
				d, err := time.ParseDuration(timeoutStr)
				if err != nil {
					return fmt.Errorf("--timeout: %w", err)
				}
				ch.Timeout = d
			} else {
				ch.Timeout = 10 * time.Second
			}
			if alertsCSV != "" {
				for _, p := range strings.Split(alertsCSV, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						ch.AlertIDs = append(ch.AlertIDs, p)
					}
				}
			}
			if resolvers, _ := cmd.Flags().GetStringSlice("resolvers"); len(resolvers) > 0 {
				for _, r := range resolvers {
					r = strings.TrimSpace(r)
					if r != "" {
						ch.Resolvers = append(ch.Resolvers, r)
					}
				}
			}
			if ctype == config.CheckHTTP {
				es, _ := cmd.Flags().GetInt("expect")
				bm, _ := cmd.Flags().GetString("body-match")
				ch.ExpectStatus = es
				ch.BodyMatch = bm
			}
			if ctype == config.CheckTLS {
				wd, _ := cmd.Flags().GetInt("warn-days")
				sni, _ := cmd.Flags().GetString("sni")
				ch.TLSWarnDays = wd
				ch.TLSServerName = strings.TrimSpace(sni)
			}
			if ctype == config.CheckDNS {
				rec, _ := cmd.Flags().GetString("record")
				res, _ := cmd.Flags().GetString("resolver")
				exp, _ := cmd.Flags().GetString("expect")
				ch.DNSRecord = strings.ToLower(strings.TrimSpace(rec))
				ch.DNSResolver = strings.TrimSpace(res)
				ch.DNSExpect = strings.TrimSpace(exp)
			}

			payload, err := json.Marshal(ch)
			if err != nil {
				return err
			}
			body := daemon.MutateBody{Kind: transport.MutationAddCheck, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "added check %s (%s) id=%s — cluster version %d\n",
				ch.Name, ch.Type, ch.ID, res.Version)
			return nil
		},
	}
	bindCheckFlags(cmd)
	return cmd
}

func bindCheckFlags(cmd *cobra.Command) {
	cmd.Flags().String("interval", "30s", "probe interval")
	cmd.Flags().String("timeout", "10s", "per-probe timeout")
	cmd.Flags().String("alerts", "", "comma-separated alert IDs/names to notify on transition")
	cmd.Flags().StringSlice("resolvers", nil, "DNS servers to resolve this check's target (e.g. 1.1.1.1,1.0.0.1). Bypasses the host's resolver cache. Tried in order with connection-level failover. Empty = use the cluster default (qu cluster resolvers show).")
}
