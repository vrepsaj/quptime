// Package main is the entry point for the qu binary.
//
// qu is a quorum-based uptime monitor. Multiple cooperating nodes
// run identical copies of this binary; they elect a master that
// owns alert dispatch and check aggregation while every node
// independently probes the configured targets.
package main

import (
	"fmt"
	"os"

	"git.cer.sh/axodouble/quptime/internal/cli"
)

// version is stamped at link time via `-ldflags "-X main.version=..."`.
// Falls back to "dev" for unreleased builds.
var version = "dev"

func main() {
	if err := cli.NewRootCommand(version).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
