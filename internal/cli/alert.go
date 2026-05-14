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

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

// bindTemplateFlags attaches --subject / --subject-file / --body /
// --body-file to a cobra command. resolveTemplateFlags reads the file
// variants (if non-empty) and returns the effective subject + body
// template strings. Inline flags take precedence over file flags.
func bindTemplateFlags(cmd *cobra.Command) {
	cmd.Flags().String("subject", "", "subject template (text/template syntax — SMTP only)")
	cmd.Flags().String("subject-file", "", "path to a file containing the subject template")
	cmd.Flags().String("body", "", "body template (text/template syntax)")
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
			fmt.Fprintln(tw, "ID\tTYPE\tDEFAULT\tNAME")
			for _, a := range cluster.Alerts {
				fmt.Fprintf(tw, "%s\t%s\t%v\t%s\n", a.ID, a.Type, a.Default, a.Name)
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

	alert.AddCommand(addParent, listCmd, removeCmd, testCmd, defaultCmd)
	root.AddCommand(alert)
}

func buildSMTPAddCmd() *cobra.Command {
	var host, user, password, from string
	var port int
	var to []string
	var startTLS, makeDefault bool

	cmd := &cobra.Command{
		Use:   "smtp <name>",
		Short: "Add an SMTP relay alert",
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
