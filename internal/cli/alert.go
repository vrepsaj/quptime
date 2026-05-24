package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/alerts"
	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

// bindTemplateFlags attaches --subject / --subject-file / --body /
// --body-file to a cobra command. resolveTemplateFlags reads the file
// variants (if non-empty) and returns the effective subject + body
// template strings. Inline flags take precedence over file flags.
func bindTemplateFlags(cmd *cobra.Command) {
	cmd.Flags().String("subject", "", "subject template, Go text/template (SMTP only; see --help for variables)")
	cmd.Flags().String("subject-file", "", "path to a file containing the subject template")
	cmd.Flags().String("body", "", "body template, Go text/template (see --help for variables)")
	cmd.Flags().String("body-file", "", "path to a file containing the body template")
}

func resolveTemplateFlags(cmd *cobra.Command) (subject, body string, err error) {
	subject, _ = cmd.Flags().GetString("subject")
	body, _ = cmd.Flags().GetString("body")
	if subject == "" {
		if p, _ := cmd.Flags().GetString("subject-file"); p != "" {
			raw, e := os.ReadFile(p)
			if e != nil {
				return "", "", fmt.Errorf("read --subject-file %s: %w", p, e)
			}
			subject = string(raw)
		}
	}
	if body == "" {
		if p, _ := cmd.Flags().GetString("body-file"); p != "" {
			raw, e := os.ReadFile(p)
			if e != nil {
				return "", "", fmt.Errorf("read --body-file %s: %w", p, e)
			}
			body = string(raw)
		}
	}
	return subject, body, nil
}

func addAlertCmd(root *cobra.Command) {
	alert := &cobra.Command{
		Use:   "alert",
		Short: "Manage notification channels",
	}

	addParent := &cobra.Command{
		Use:   "add",
		Short: "Add a new alert channel",
	}
	addParent.AddCommand(buildSMTPAddCmd(), buildDiscordAddCmd())

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List configured alerts",
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, err := config.LoadClusterConfig()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTYPE\tENABLED\tDEFAULT\tNAME")
			for _, a := range cluster.Alerts {
				fmt.Fprintf(tw, "%s\t%s\t%v\t%v\t%s\n", a.ID, a.Type, !a.Disabled, a.Default, a.Name)
			}
			return tw.Flush()
		},
	}

	removeCmd := &cobra.Command{
		Use:   "remove <id-or-name>",
		Short: "Remove an alert channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			payload, _ := json.Marshal(args[0])
			body := daemon.MutateBody{Kind: transport.MutationRemoveAlert, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "removed alert %s (cluster version now %d)\n", args[0], res.Version)
			return nil
		},
	}

	testCmd := &cobra.Command{
		Use:   "test <id-or-name>",
		Short: "Send a test notification through an alert channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			body := daemon.AlertTestBody{AlertID: args[0]}
			if _, err := callDaemon(ctx, daemon.CtrlAlertTest, body); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "test alert sent via %s\n", args[0])
			return nil
		},
	}

	defaultCmd := &cobra.Command{
		Use:   "default <id-or-name> <on|off>",
		Short: "Toggle whether an alert is attached to every check by default",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			var on bool
			switch args[1] {
			case "on", "true", "yes", "1":
				on = true
			case "off", "false", "no", "0":
				on = false
			default:
				return fmt.Errorf("second arg must be on/off, got %q", args[1])
			}
			cluster, err := config.LoadClusterConfig()
			if err != nil {
				return err
			}
			existing := cluster.FindAlert(args[0])
			if existing == nil {
				return fmt.Errorf("no alert named %q", args[0])
			}
			existing.Default = on
			payload, _ := json.Marshal(existing)
			body := daemon.MutateBody{Kind: transport.MutationAddAlert, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "alert %s default=%v — cluster version %d\n",
				existing.Name, on, res.Version)
			return nil
		},
	}

	enableCmd := buildAlertToggleCmd("enable", false,
		"Re-enable a silenced alert so it fires on transitions again")
	disableCmd := buildAlertToggleCmd("disable", true,
		"Silence an alert: it stops firing on transitions and is dropped from defaults")

	alert.AddCommand(addParent, listCmd, removeCmd, testCmd, defaultCmd, enableCmd, disableCmd, buildAlertEditCmd())
	root.AddCommand(alert)
}

// buildAlertToggleCmd returns the `qu alert enable|disable` subcommand.
// Both share an implementation: look up the alert, flip Disabled, and
// re-submit it through the standard AddAlert mutation (which replaces
// any existing entry with matching ID).
func buildAlertToggleCmd(use string, disabled bool, short string) *cobra.Command {
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
			existing := cluster.FindAlert(args[0])
			if existing == nil {
				return fmt.Errorf("no alert named %q", args[0])
			}
			if existing.Disabled == disabled {
				fmt.Fprintf(cmd.OutOrStdout(), "alert %s already %sd\n", existing.Name, use)
				return nil
			}
			existing.Disabled = disabled
			payload, err := json.Marshal(existing)
			if err != nil {
				return err
			}
			body := daemon.MutateBody{Kind: transport.MutationAddAlert, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "%sd alert %s (cluster version now %d)\n",
				use, existing.Name, res.Version)
			return nil
		},
	}
}

// buildAlertEditCmd returns `qu alert edit`, which updates fields of an
// existing alert. Only flags actually passed take effect. The alert's
// type cannot be changed (would require re-validating type-specific
// fields end-to-end); delete and re-add instead if you need to switch
// from SMTP to Discord or vice versa.
func buildAlertEditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <id-or-name>",
		Short: "Update fields of an existing alert channel",
		Long: `Update one or more fields of an existing alert. Only flags you pass
take effect; everything else is preserved.

The type (smtp/discord) cannot be changed in place — delete and re-add
the alert if you need to switch channels.

` + alerts.TemplateVarsHelp(),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()

			cluster, err := config.LoadClusterConfig()
			if err != nil {
				return err
			}
			existing := cluster.FindAlert(args[0])
			if existing == nil {
				return fmt.Errorf("no alert named %q", args[0])
			}

			f := cmd.Flags()
			if f.Changed("name") {
				v, _ := f.GetString("name")
				existing.Name = v
			}
			if f.Changed("default") {
				v, _ := f.GetBool("default")
				existing.Default = v
			}
			// Templates: inline flag wins over file flag. Either changing
			// applies; passing an empty inline string clears the template.
			if f.Changed("subject") {
				v, _ := f.GetString("subject")
				existing.SubjectTemplate = v
			} else if f.Changed("subject-file") {
				p, _ := f.GetString("subject-file")
				if p != "" {
					raw, e := os.ReadFile(p)
					if e != nil {
						return fmt.Errorf("read --subject-file %s: %w", p, e)
					}
					existing.SubjectTemplate = string(raw)
				}
			}
			if f.Changed("body") {
				v, _ := f.GetString("body")
				existing.BodyTemplate = v
			} else if f.Changed("body-file") {
				p, _ := f.GetString("body-file")
				if p != "" {
					raw, e := os.ReadFile(p)
					if e != nil {
						return fmt.Errorf("read --body-file %s: %w", p, e)
					}
					existing.BodyTemplate = string(raw)
				}
			}

			switch existing.Type {
			case config.AlertSMTP:
				if f.Changed("webhook") {
					return fmt.Errorf("--webhook only applies to Discord alerts")
				}
				if f.Changed("host") {
					v, _ := f.GetString("host")
					existing.SMTPHost = v
				}
				if f.Changed("port") {
					v, _ := f.GetInt("port")
					existing.SMTPPort = v
				}
				if f.Changed("user") {
					v, _ := f.GetString("user")
					existing.SMTPUser = v
				}
				if f.Changed("password") {
					v, _ := f.GetString("password")
					existing.SMTPPassword = v
				}
				if f.Changed("from") {
					v, _ := f.GetString("from")
					existing.SMTPFrom = v
				}
				if f.Changed("to") {
					v, _ := f.GetStringSlice("to")
					existing.SMTPTo = v
				}
				if f.Changed("starttls") {
					v, _ := f.GetBool("starttls")
					existing.SMTPStartTLS = v
				}
			case config.AlertDiscord:
				for _, smtpFlag := range []string{"host", "port", "user", "password", "from", "to", "starttls"} {
					if f.Changed(smtpFlag) {
						return fmt.Errorf("--%s only applies to SMTP alerts", smtpFlag)
					}
				}
				if f.Changed("webhook") {
					v, _ := f.GetString("webhook")
					existing.DiscordWebhook = v
				}
			}

			payload, err := json.Marshal(existing)
			if err != nil {
				return err
			}
			body := daemon.MutateBody{Kind: transport.MutationAddAlert, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "updated alert %s (cluster version now %d)\n", existing.Name, res.Version)
			return nil
		},
	}
	cmd.Flags().String("name", "", "rename the alert")
	cmd.Flags().Bool("default", false, "attach to every check automatically")
	cmd.Flags().String("host", "", "SMTP server host (SMTP only)")
	cmd.Flags().Int("port", 587, "SMTP server port (SMTP only)")
	cmd.Flags().String("user", "", "SMTP auth user (SMTP only)")
	cmd.Flags().String("password", "", "SMTP auth password (SMTP only)")
	cmd.Flags().String("from", "", "envelope From address (SMTP only)")
	cmd.Flags().StringSlice("to", nil, "recipient address, repeatable (SMTP only)")
	cmd.Flags().Bool("starttls", true, "negotiate STARTTLS (SMTP only)")
	cmd.Flags().String("webhook", "", "Discord webhook URL (Discord only)")
	bindTemplateFlags(cmd)
	return cmd
}

func buildSMTPAddCmd() *cobra.Command {
	var host, user, password, from string
	var port int
	var to []string
	var startTLS, makeDefault bool

	cmd := &cobra.Command{
		Use:   "smtp <name>",
		Short: "Add an SMTP relay alert",
		Long:  "Add an SMTP relay alert.\n\n" + alerts.TemplateVarsHelp(),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			subj, body, err := resolveTemplateFlags(cmd)
			if err != nil {
				return err
			}
			a := config.Alert{
				ID:              uuid.NewString(),
				Name:            args[0],
				Type:            config.AlertSMTP,
				Default:         makeDefault,
				SubjectTemplate: subj,
				BodyTemplate:    body,
				SMTPHost:        host,
				SMTPPort:        port,
				SMTPUser:        user,
				SMTPPassword:    password,
				SMTPFrom:        from,
				SMTPTo:          to,
				SMTPStartTLS:    startTLS,
			}
			payload, _ := json.Marshal(a)
			mb := daemon.MutateBody{Kind: transport.MutationAddAlert, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, mb)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "added smtp alert %s id=%s — cluster version %d\n",
				a.Name, a.ID, res.Version)
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "smtp server host")
	cmd.Flags().IntVar(&port, "port", 587, "smtp server port")
	cmd.Flags().StringVar(&user, "user", "", "smtp auth user (empty for anonymous)")
	cmd.Flags().StringVar(&password, "password", "", "smtp auth password")
	cmd.Flags().StringVar(&from, "from", "", "envelope From address")
	cmd.Flags().StringSliceVar(&to, "to", nil, "recipient address (repeat or comma-separate)")
	cmd.Flags().BoolVar(&startTLS, "starttls", true, "negotiate STARTTLS")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "attach this alert to every check automatically")
	bindTemplateFlags(cmd)
	_ = cmd.MarkFlagRequired("host")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func buildDiscordAddCmd() *cobra.Command {
	var webhook string
	var makeDefault bool
	cmd := &cobra.Command{
		Use:   "discord <name>",
		Short: "Add a Discord webhook alert",
		Long:  "Add a Discord webhook alert.\n\n" + alerts.TemplateVarsHelp(),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			subj, body, err := resolveTemplateFlags(cmd)
			if err != nil {
				return err
			}
			a := config.Alert{
				ID:              uuid.NewString(),
				Name:            args[0],
				Type:            config.AlertDiscord,
				Default:         makeDefault,
				SubjectTemplate: subj,
				BodyTemplate:    body,
				DiscordWebhook:  webhook,
			}
			payload, _ := json.Marshal(a)
			mb := daemon.MutateBody{Kind: transport.MutationAddAlert, Payload: payload}
			raw, err := callDaemon(ctx, daemon.CtrlMutate, mb)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "added discord alert %s id=%s — cluster version %d\n",
				a.Name, a.ID, res.Version)
			return nil
		},
	}
	cmd.Flags().StringVar(&webhook, "webhook", "", "discord webhook URL")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "attach this alert to every check automatically")
	bindTemplateFlags(cmd)
	_ = cmd.MarkFlagRequired("webhook")
	return cmd
}
