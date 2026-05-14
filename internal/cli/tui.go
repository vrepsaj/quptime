package cli

import (
	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/tui"
)

func addTUICmd(root *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the interactive terminal UI",
		Long: "Open a full-screen TUI that overlays the same commands the CLI offers.\n" +
			"The TUI is a thin client over the local daemon socket — start the daemon\n" +
			"with `qu serve` before running this.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run()
		},
	}
	root.AddCommand(cmd)
}
