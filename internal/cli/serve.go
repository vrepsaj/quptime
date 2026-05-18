package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/daemon"
)

func addServeCmd(root *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the qu daemon in the foreground",
		Long: `Run the qu daemon: starts the inter-node listener, the local
control socket for the CLI, the heartbeat loop and the check
scheduler. Stops cleanly on SIGINT or SIGTERM.

If node.yaml does not exist yet, serve will bootstrap it using values
from the QUPTIME_* environment variables (see docs/configuration.md).
This makes a single ` + "`docker compose up`" + ` enough to launch a new node —
no separate ` + "`qu init`" + ` step is required when the data volume is
fresh.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := log.New(os.Stderr, "quptime: ", log.LstdFlags|log.Lmsgprefix)
			if err := autoInitIfNeeded(cmd, logger); err != nil {
				return err
			}
			d, err := daemon.New(logger)
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return d.Run(ctx)
		},
	}
	root.AddCommand(cmd)
}

// autoInitIfNeeded bootstraps the node on first launch.
//
// Friction this removes for container deploys: before, the operator
// had to `docker compose run --rm quptime init …` once before the
// service could come up, which makes `restart: unless-stopped`
// awkward and forces an out-of-band step into every fresh volume.
// Now serve auto-runs the same bootstrap path using QUPTIME_* env
// vars when node.yaml is absent, so the compose file can come up on
// the first try.
//
// Pre-existing node.yaml is left untouched — we only bootstrap when
// the file is genuinely missing. Any other stat error (permission
// denied, broken symlink) is surfaced so the operator sees the real
// problem instead of a confused auto-init attempt clobbering state.
func autoInitIfNeeded(cmd *cobra.Command, logger *log.Logger) error {
	_, err := os.Stat(config.NodeFilePath())
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat node.yaml: %w", err)
	}

	logger.Printf("node.yaml not found at %s — bootstrapping from environment", config.NodeFilePath())
	n := &config.NodeConfig{}
	if err := n.ApplyEnvOverrides(); err != nil {
		return err
	}
	if _, err := bootstrapNode(n); err != nil {
		return fmt.Errorf("auto-init: %w", err)
	}
	printBootstrapResult(cmd.OutOrStderr(), n)
	return nil
}
