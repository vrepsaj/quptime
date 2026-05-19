package cli

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/alerts"
)

//go:embed builder.html
var builderHTML string

// addBuilderCmd registers `qu builder`, which materialises a standalone
// HTML page that operators can use to compose alert message templates
// interactively. The page is self-contained — no network calls, no
// external assets — so it can be opened straight from disk or copied
// to a workstation that has no qu binary installed.
//
// The default templates baked into the page are injected from
// internal/alerts at build (actually at command-run) time, so the
// builder always offers the same starting points the daemon would
// render itself.
func addBuilderCmd(root *cobra.Command) {
	var output string
	cmd := &cobra.Command{
		Use:   "builder",
		Short: "Generate a standalone HTML alert-template builder",
		Long: `Write a self-contained HTML page that helps you compose subject and
body templates for ` + "`qu alert`" + ` channels. The page bundles a
drag-and-drop variable palette, a live preview rendered against
editable sample data, and the per-check-type defaults you can use as a
starting point. It runs entirely offline once written to disk.

Pass ` + "`-o -`" + ` to write the HTML to stdout instead of a file.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			rendered, err := renderBuilderHTML()
			if err != nil {
				return err
			}
			if output == "-" {
				_, err := cmd.OutOrStdout().Write([]byte(rendered))
				return err
			}
			if err := os.WriteFile(output, []byte(rendered), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", output, err)
			}
			abs, _ := filepath.Abs(output)
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", abs)
			fmt.Fprintf(cmd.OutOrStdout(), "open it in a browser to start composing templates.\n")
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "quptime-template-builder.html",
		"output path for the generated HTML (use - for stdout)")
	root.AddCommand(cmd)
}

// renderBuilderHTML substitutes the placeholder tokens in builder.html
// with JSON-encoded copies of the current default templates. JSON
// encoding is exactly what we want for JS string literals — it handles
// the embedded backticks, backslashes, and newlines in the Go template
// constants without further escaping.
func renderBuilderHTML() (string, error) {
	subs := map[string]string{
		"__QU_DEFAULT_HTTP_SUBJECT__":    alerts.DefaultSubjectHTTP,
		"__QU_DEFAULT_HTTP_BODY__":       alerts.DefaultBodyHTTP,
		"__QU_DEFAULT_TLS_SUBJECT__":     alerts.DefaultSubjectTLS,
		"__QU_DEFAULT_TLS_BODY__":        alerts.DefaultBodyTLS,
		"__QU_DEFAULT_TCP_SUBJECT__":     alerts.DefaultSubjectTCP,
		"__QU_DEFAULT_TCP_BODY__":        alerts.DefaultBodyTCP,
		"__QU_DEFAULT_ICMP_SUBJECT__":    alerts.DefaultSubjectICMP,
		"__QU_DEFAULT_ICMP_BODY__":       alerts.DefaultBodyICMP,
		"__QU_DEFAULT_DNS_SUBJECT__":     alerts.DefaultSubjectDNS,
		"__QU_DEFAULT_DNS_BODY__":        alerts.DefaultBodyDNS,
		"__QU_DEFAULT_GENERIC_SUBJECT__": alerts.DefaultSubjectGeneric,
		"__QU_DEFAULT_GENERIC_BODY__":    alerts.DefaultBodyGeneric,
	}
	out := builderHTML
	for marker, value := range subs {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("encode %s: %w", marker, err)
		}
		if !strings.Contains(out, marker) {
			return "", fmt.Errorf("builder.html missing placeholder %s", marker)
		}
		out = strings.ReplaceAll(out, marker, string(encoded))
	}
	return out, nil
}
