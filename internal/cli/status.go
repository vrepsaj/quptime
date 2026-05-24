package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

func addStatusCmd(root *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print quorum, master, and check state",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			return runStatusPrint(ctx, cmd, false)
		},
	}
	root.AddCommand(cmd)
}

// runStatusPrint fetches /status from the daemon and prints either
// the peer view or the full view depending on peersOnly.
func runStatusPrint(ctx context.Context, cmd *cobra.Command, peersOnly bool) error {
	raw, err := callDaemon(ctx, daemon.CtrlStatus, nil)
	if err != nil {
		return err
	}
	var st transport.StatusResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "node       %s\n", st.NodeID)
	fmt.Fprintf(out, "term       %d\n", st.Term)
	fmt.Fprintf(out, "master     %s\n", masterOrNone(st.MasterID))
	fmt.Fprintf(out, "quorum     %v (need %d)\n", st.HasQuorum, st.QuorumSize)
	fmt.Fprintf(out, "config ver %d\n", st.Version)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "PEERS")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tNODE_ID\tADVERTISE\tLIVE\tLAST_SEEN")
	for _, p := range st.Peers {
		lastSeen := "-"
		if !p.LastSeen.IsZero() {
			lastSeen = p.LastSeen.Format(time.RFC3339)
		}
		marker := " "
		if p.NodeID == st.NodeID {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", marker, p.NodeID, p.Advertise, p.Live, lastSeen)
	}
	tw.Flush()
	fmt.Fprintln(out, "(* = this node)")

	if peersOnly {
		return nil
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "CHECKS")
	tw2 := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw2, "ID\tNAME\tSTATE\tOK/TOTAL\tALERTS\tDETAIL")
	for _, c := range st.Checks {
		fmt.Fprintf(tw2, "%s\t%s\t%s\t%d/%d\t%s\t%s\n",
			c.CheckID, c.Name, stateCol(c.State, c.Disabled), c.OKCount, c.Total,
			alertsCol(c.Alerts), c.Detail)
	}
	if err := tw2.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "(alerts marked * are attached as defaults)")
	return nil
}

// stateCol prepends "(disabled) " to a check's runtime state when the
// operator has paused it, so list and status outputs surface the paused
// status without adding a separate column.
func stateCol(state string, disabled bool) string {
	if disabled {
		return "(disabled) " + state
	}
	return state
}

func alertsCol(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}

// runStatusPrintChecks renders only the checks block (used by
// `qu check list`).
func runStatusPrintChecks(ctx context.Context, cmd *cobra.Command) error {
	raw, err := callDaemon(ctx, daemon.CtrlStatus, nil)
	if err != nil {
		return err
	}
	var st transport.StatusResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tOK/TOTAL\tALERTS\tDETAIL")
	for _, c := range st.Checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\n",
			c.CheckID, c.Name, stateCol(c.State, c.Disabled), c.OKCount, c.Total,
			alertsCol(c.Alerts), c.Detail)
	}
	return tw.Flush()
}

func masterOrNone(id string) string {
	if id == "" {
		return "(none — no quorum or election in progress)"
	}
	return id
}
