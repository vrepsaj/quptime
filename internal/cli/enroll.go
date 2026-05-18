package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/crypto"
	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
	"git.cer.sh/axodouble/quptime/internal/trust"
)

// addEnrollCmd wires up `qu enroll …`. The subcommands fall into two
// groups by which side of the cluster they run on:
//
//   - create / list / approve / revoke run on an existing cluster
//     node and talk to its local daemon socket like every other
//     `qu …` command.
//   - join runs on a fresh host that does not yet have a daemon.
//     It generates local identity, dials a cluster endpoint pinned
//     by the token, and submits the enrollment over inter-node mTLS
//     directly. No local daemon required.
func addEnrollCmd(root *cobra.Command) {
	enroll := &cobra.Command{
		Use:   "enroll",
		Short: "Issue and redeem pre-deployment enrollment tokens",
		Long: `Pre-deployment enrollment replaces the cluster-secret model.

A cluster operator generates a single-use token with ` + "`qu enroll create`" + `;
the resulting one-line ` + "`qu enroll join <token>`" + ` is run on the new
host. Trust is acquired from both sides: the joiner pins the cluster's
TLS fingerprint via the token, and the cluster either auto-approves
(when --auto-approve was set at creation time) or requires
` + "`qu enroll approve`" + ` before the new node becomes a peer.`,
	}

	enroll.AddCommand(buildEnrollCreateCmd())
	enroll.AddCommand(buildEnrollListCmd())
	enroll.AddCommand(buildEnrollApproveCmd())
	enroll.AddCommand(buildEnrollRevokeCmd())
	enroll.AddCommand(buildEnrollJoinCmd())

	root.AddCommand(enroll)
}

func buildEnrollCreateCmd() *cobra.Command {
	var (
		name        string
		ttl         time.Duration
		autoApprove bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a pre-deployment enrollment token",
		Long: `Generate a one-shot enrollment token bound to this cluster.

The token embeds the cluster's contact endpoints and their TLS
fingerprints, so the joiner verifies it is talking to the right
cluster before sending anything secret. The cluster verifies the
joiner by hashing the token-secret and constant-time comparing
against the stored hash in cluster.yaml.

Without --auto-approve, a successful submission parks the joiner
under "pending"; an operator must then run ` + "`qu enroll approve <id>`" + `
to commit the new node as a peer. With --auto-approve, redemption
immediately adds the joiner — useful for unattended provisioning.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			body := daemon.EnrollCreateBody{
				Name:        strings.TrimSpace(name),
				TTL:         ttl,
				AutoApprove: autoApprove,
			}
			raw, err := callDaemon(ctx, daemon.CtrlEnrollCreate, body)
			if err != nil {
				return err
			}
			var res daemon.EnrollCreateResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			printEnrollCreateResult(cmd.OutOrStdout(), res)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "optional human label for the token (e.g. host name)")
	cmd.Flags().DurationVar(&ttl, "ttl", 1*time.Hour, "how long the token remains valid")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "skip the cluster-side approval step (joiner becomes a peer on submission)")
	return cmd
}

func buildEnrollListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List outstanding enrollment tokens and pending approvals",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			raw, err := callDaemon(ctx, daemon.CtrlEnrollList, nil)
			if err != nil {
				return err
			}
			var res daemon.EnrollListResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			printEnrollList(cmd.OutOrStdout(), res)
			return nil
		},
	}
}

func buildEnrollApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <id-or-name>",
		Short: "Approve a pending enrollment (manual-approval tokens only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			body := daemon.EnrollTargetBody{ID: args[0]}
			raw, err := callDaemon(ctx, daemon.CtrlEnrollApprove, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "approved %s (cluster version now %d)\n", args[0], res.Version)
			return nil
		},
	}
}

func buildEnrollRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id-or-name>",
		Short: "Revoke an outstanding enrollment token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			body := daemon.EnrollTargetBody{ID: args[0]}
			raw, err := callDaemon(ctx, daemon.CtrlEnrollRevoke, body)
			if err != nil {
				return err
			}
			var res daemon.MutateResult
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s (cluster version now %d)\n", args[0], res.Version)
			return nil
		},
	}
}

func buildEnrollJoinCmd() *cobra.Command {
	var (
		advertise string
		bindAddr  string
		bindPort  int
		yes       bool
	)
	cmd := &cobra.Command{
		Use:   "join <token>",
		Short: "Redeem an enrollment token on a fresh node",
		Long: `Run on the new host. Initialises node identity (if not already
done), dials the cluster using the contact hints baked into the
token, and submits the enrollment over mTLS. The joiner verifies
the cluster's TLS fingerprint against the token before sending
anything secret.

If the token was issued with --auto-approve, the host is a full
member on success — just start the daemon. Otherwise the request
is recorded as pending and an operator must run
` + "`qu enroll approve <id>`" + ` on the cluster before the daemon will
form quorum.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			return runEnrollJoin(ctx, cmd, args[0], advertise, bindAddr, bindPort, yes)
		},
	}
	cmd.Flags().StringVar(&advertise, "advertise", "", "address peers should use to reach this node (host:port)")
	cmd.Flags().StringVar(&bindAddr, "bind", "0.0.0.0", "listen address for inter-node traffic")
	cmd.Flags().IntVar(&bindPort, "port", 9901, "listen port for inter-node traffic")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the cluster-fingerprint confirmation prompt")
	return cmd
}

// runEnrollJoin orchestrates the joiner side of the dance.
//
//  1. Refuse if node.yaml already exists (we'd overwrite identity).
//  2. Decode the token; check it isn't already expired client-side.
//  3. bootstrapNode generates the local identity, keypair, and
//     self-signed cert. This populates node.yaml + keys/ but does NOT
//     seed cluster.yaml from a stale source — we'll get the cluster
//     snapshot from the cluster's response.
//  4. For each cluster endpoint in the token: TOFU-fetch the peer's
//     cert and check fingerprint == token's expected fingerprint. The
//     first match wins.
//  5. Confirm with the operator (skip with --yes).
//  6. Add the verified peer to the local trust store so transport.Client
//     can dial it on the normal (trusted) path.
//  7. Submit EnrollRequest. On Accepted, persist every returned peer
//     into the trust store. On Pending, we already trust the bootstrap
//     peer; that's enough for `qu serve` to catch up via heartbeats
//     once approval lands.
func runEnrollJoin(ctx context.Context, cmd *cobra.Command, tokenStr, advertise, bindAddr string, bindPort int, yes bool) error {
	out := cmd.OutOrStdout()

	if _, err := os.Stat(config.NodeFilePath()); err == nil {
		return fmt.Errorf("node.yaml already exists at %s — refusing to overwrite; wipe the data dir first to re-enroll", config.NodeFilePath())
	}

	payload, err := daemon.DecodeEnrollmentToken(tokenStr)
	if err != nil {
		return fmt.Errorf("decode token: %w", err)
	}
	if !payload.ExpiresAt.IsZero() && time.Now().After(payload.ExpiresAt) {
		return errors.New("token has already expired — ask the cluster operator for a fresh one")
	}

	nodeCfg := &config.NodeConfig{
		BindAddr:  bindAddr,
		BindPort:  bindPort,
		Advertise: advertise,
	}
	if _, err := bootstrapNode(nodeCfg); err != nil {
		return fmt.Errorf("bootstrap local identity: %w", err)
	}
	fmt.Fprintf(out, "generated local identity: %s\n", nodeCfg.NodeID)
	fmt.Fprintf(out, "advertise:                %s\n", nodeCfg.AdvertiseAddr())

	priv, err := crypto.LoadPrivateKey()
	if err != nil {
		return fmt.Errorf("load private key: %w", err)
	}
	myCertPEM, err := crypto.LoadCertPEM()
	if err != nil {
		return fmt.Errorf("load cert: %w", err)
	}
	myFP, err := crypto.FingerprintFromCertPEM(myCertPEM)
	if err != nil {
		return fmt.Errorf("fingerprint own cert: %w", err)
	}

	store, err := trust.Load()
	if err != nil {
		return fmt.Errorf("load trust.yaml: %w", err)
	}
	assets := &transport.TLSAssets{Cert: myCertPEM, Key: priv, Trust: store}

	var (
		boundEndpoint daemon.EnrollEndpoint
		bootstrapCert *transport.PeerCertSample
		peerNodeID    string
	)
	for _, ep := range payload.Endpoints {
		sample, err := transport.FetchPeerCert(ctx, assets, ep.Advertise)
		if err != nil {
			fmt.Fprintf(out, "warn: cannot reach %s: %v\n", ep.Advertise, err)
			continue
		}
		if sample.Fingerprint != ep.Fingerprint {
			fmt.Fprintf(out, "warn: %s presented fingerprint %s, token expected %s — skipping\n",
				ep.Advertise, sample.Fingerprint, ep.Fingerprint)
			continue
		}
		boundEndpoint = ep
		bootstrapCert = sample
		peerNodeID = sample.Cert.Subject.CommonName
		break
	}
	if bootstrapCert == nil {
		return errors.New("no token endpoint answered with the expected fingerprint — token may be stale or the cluster has rotated certs")
	}
	if peerNodeID == "" {
		return errors.New("cluster peer cert has empty CommonName; cannot identify it")
	}

	fmt.Fprintf(out, "cluster endpoint:         %s\n", boundEndpoint.Advertise)
	fmt.Fprintf(out, "cluster node id:          %s\n", peerNodeID)
	fmt.Fprintf(out, "cluster fingerprint:      %s  (verified against token)\n", bootstrapCert.Fingerprint)

	if !yes {
		fmt.Fprint(out, "submit enrollment to this cluster? [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans != "y" && ans != "yes" {
			return errors.New("aborted by operator")
		}
	}

	if err := store.Add(trust.Entry{
		NodeID:      peerNodeID,
		Address:     boundEndpoint.Advertise,
		Fingerprint: bootstrapCert.Fingerprint,
		CertPEM:     string(bootstrapCert.CertPEM),
	}); err != nil {
		return fmt.Errorf("trust bootstrap peer: %w", err)
	}

	client := transport.NewClient(assets)
	defer client.Close()
	req := transport.EnrollRequest{
		TokenID:     payload.ID,
		TokenSecret: payload.Secret,
		NodeID:      nodeCfg.NodeID,
		Advertise:   nodeCfg.AdvertiseAddr(),
		Fingerprint: myFP,
		CertPEM:     string(myCertPEM),
	}
	var resp transport.EnrollResponse
	if err := client.Call(ctx, peerNodeID, boundEndpoint.Advertise, transport.MethodEnroll, req, &resp); err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("cluster rejected enrollment: %s", resp.Error)
	}

	// Seed the joiner's cluster.yaml with at least the bootstrap peer
	// so `qu serve` has someone to heartbeat with. Without this, the
	// quorum manager would only see self (from bootstrapNode's seed)
	// and never reach out to the cluster — the joiner would sit dark
	// forever even after operator approval.
	bootstrapPeer := config.PeerInfo{
		NodeID:      peerNodeID,
		Advertise:   boundEndpoint.Advertise,
		Fingerprint: bootstrapCert.Fingerprint,
		CertPEM:     string(bootstrapCert.CertPEM),
	}

	if resp.Pending {
		if err := seedJoinerClusterYaml(nodeCfg, myCertPEM, []config.PeerInfo{bootstrapPeer}); err != nil {
			fmt.Fprintf(out, "warn: seed cluster.yaml: %v\n", err)
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "enrollment submitted; waiting for cluster-side approval.")
		fmt.Fprintln(out, "the cluster operator should run:")
		fmt.Fprintf(out, "    qu enroll approve %s\n", payload.ID)
		fmt.Fprintln(out, "you can start `qu serve` now or after approval — the daemon will")
		fmt.Fprintln(out, "fail to form quorum until approval lands and then catch up via heartbeats.")
		return nil
	}

	if !resp.Accepted {
		return errors.New("cluster returned neither accepted nor pending")
	}

	// Auto-approved path: persist trust entries for every existing
	// peer the cluster handed back, and seed cluster.yaml so quorum
	// management has the full peer list immediately on `qu serve`.
	peers := []config.PeerInfo{bootstrapPeer}
	added := 0
	for _, p := range transport.EnrollSummaryPeers(resp.Cluster) {
		fp, err := crypto.FingerprintFromCertPEM([]byte(p.CertPEM))
		if err != nil || fp != p.Fingerprint {
			fmt.Fprintf(out, "warn: skipping peer %s — fingerprint mismatch in response\n", p.NodeID)
			continue
		}
		if err := store.Add(trust.Entry{
			NodeID:      p.NodeID,
			Address:     p.Advertise,
			Fingerprint: p.Fingerprint,
			CertPEM:     p.CertPEM,
		}); err != nil {
			fmt.Fprintf(out, "warn: trust add %s: %v\n", p.NodeID, err)
			continue
		}
		if p.NodeID != bootstrapPeer.NodeID {
			peers = append(peers, config.PeerInfo{
				NodeID:      p.NodeID,
				Advertise:   p.Advertise,
				Fingerprint: p.Fingerprint,
				CertPEM:     p.CertPEM,
			})
		}
		added++
	}
	if err := seedJoinerClusterYaml(nodeCfg, myCertPEM, peers); err != nil {
		fmt.Fprintf(out, "warn: seed cluster.yaml: %v\n", err)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "enrollment accepted — trusted %d existing cluster peer(s)\n", added)
	fmt.Fprintln(out, "next: `qu serve` (or restart the systemd unit if installed)")
	return nil
}

// seedJoinerClusterYaml rewrites the joiner's local cluster.yaml so
// it carries self + every peer the joiner has learned about. Without
// this the quorum manager would only know about self (from
// bootstrapNode's seed) and never heartbeat outward, leaving the new
// node permanently disconnected from the cluster.
//
// The on-disk version is bumped from whatever bootstrapNode wrote
// (Version=1) so the file is recognisably "post-enrolment" — the
// first real broadcast from master (whatever version it carries)
// will still supersede it because Replace gates on strict-greater.
func seedJoinerClusterYaml(nodeCfg *config.NodeConfig, myCertPEM []byte, peers []config.PeerInfo) error {
	selfFP, err := crypto.FingerprintFromCertPEM(myCertPEM)
	if err != nil {
		return err
	}
	cluster, err := config.LoadClusterConfig()
	if err != nil {
		return err
	}
	return cluster.Mutate(nodeCfg.NodeID, func(c *config.ClusterConfig) error {
		c.Peers = []config.PeerInfo{{
			NodeID:      nodeCfg.NodeID,
			Advertise:   nodeCfg.AdvertiseAddr(),
			Fingerprint: selfFP,
			CertPEM:     string(myCertPEM),
		}}
		seen := map[string]bool{nodeCfg.NodeID: true}
		for _, p := range peers {
			if seen[p.NodeID] {
				continue
			}
			c.Peers = append(c.Peers, p)
			seen[p.NodeID] = true
		}
		return nil
	})
}

func printEnrollCreateResult(w io.Writer, res daemon.EnrollCreateResult) {
	fmt.Fprintln(w, "enrollment token created.")
	fmt.Fprintf(w, "  id          : %s\n", res.ID)
	if res.Name != "" {
		fmt.Fprintf(w, "  name        : %s\n", res.Name)
	}
	fmt.Fprintf(w, "  expires at  : %s  (%s from now)\n",
		res.ExpiresAt.Format(time.RFC3339),
		time.Until(res.ExpiresAt).Round(time.Second))
	if res.AutoApprove {
		fmt.Fprintln(w, "  approval    : auto (joiner becomes a peer on submission)")
	} else {
		fmt.Fprintln(w, "  approval    : manual (run `qu enroll approve` after submission)")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "run on the new host:")
	fmt.Fprintf(w, "  qu enroll join %s\n", res.Token)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "the token is single-use; revoke with `qu enroll revoke %s`.\n", res.ID)
}

func printEnrollList(w io.Writer, res daemon.EnrollListResult) {
	if len(res.Entries) == 0 {
		fmt.Fprintln(w, "no outstanding enrollment tokens.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tAPPROVAL\tEXPIRES\tPENDING JOINER")
	now := time.Now()
	for _, e := range res.Entries {
		approval := "manual"
		if e.AutoApprove {
			approval = "auto"
		}
		expires := "-"
		if !e.ExpiresAt.IsZero() {
			if e.ExpiresAt.Before(now) {
				expires = "EXPIRED"
			} else {
				expires = time.Until(e.ExpiresAt).Round(time.Second).String()
			}
		}
		pending := "-"
		if e.Pending != nil {
			pending = fmt.Sprintf("%s @ %s", shortID(e.Pending.NodeID), e.Pending.Advertise)
		}
		name := e.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			shortID(e.ID), name, approval, expires, pending)
	}
	_ = tw.Flush()
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
