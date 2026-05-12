package cli

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/crypto"
)

func addInitCmd(root *cobra.Command) {
	var advertise string
	var bindAddr string
	var bindPort int
	var clusterSecret string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate node identity, keys, and config",
		Long: `Initialise a new qu node on this host: pick a UUID, generate an
RSA keypair, write a default node.yaml, and prepare the trust store.

Pass --secret on every subsequent node so they share the same
cluster join secret. If --secret is omitted on the very first node, a
random secret is generated and printed for the operator to copy.

Idempotent in one direction only: existing key material is never
overwritten. Re-run only after wiping the data directory.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := config.EnsureDataDir(); err != nil {
				return err
			}
			if _, err := os.Stat(config.NodeFilePath()); err == nil {
				return errors.New("node.yaml already exists in data dir — refusing to overwrite")
			}

			secret := clusterSecret
			generated := false
			if secret == "" {
				s, err := generateSecret()
				if err != nil {
					return fmt.Errorf("generate cluster secret: %w", err)
				}
				secret = s
				generated = true
			}

			nodeID := uuid.NewString()
			n := &config.NodeConfig{
				NodeID:        nodeID,
				BindAddr:      bindAddr,
				BindPort:      bindPort,
				Advertise:     advertise,
				ClusterSecret: secret,
			}
			if err := n.Save(); err != nil {
				return fmt.Errorf("save node.yaml: %w", err)
			}
			if _, err := crypto.GenerateKeyPair(nodeID); err != nil {
				return fmt.Errorf("generate keys: %w", err)
			}

			// Seed cluster.yaml with this node as its own first peer.
			// Without this the math in `quorum` would treat a one-node
			// cluster as "0 peers, fallback quorum=1, master=self" —
			// which works in isolation but breaks the moment another
			// node joins, because the replicated peers list would lack
			// the inviter, leading to split-brain elections.
			certPEM, err := crypto.LoadCertPEM()
			if err != nil {
				return fmt.Errorf("load cert: %w", err)
			}
			fp, err := crypto.FingerprintFromCertPEM(certPEM)
			if err != nil {
				return fmt.Errorf("fingerprint own cert: %w", err)
			}
			cluster := &config.ClusterConfig{}
			if err := cluster.Mutate(nodeID, func(c *config.ClusterConfig) error {
				c.Peers = []config.PeerInfo{{
					NodeID:      nodeID,
					Advertise:   n.AdvertiseAddr(),
					Fingerprint: fp,
					CertPEM:     string(certPEM),
				}}
				return nil
			}); err != nil {
				return fmt.Errorf("seed cluster.yaml: %w", err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "initialised node %s\n", nodeID)
			fmt.Fprintf(out, "data dir: %s\n", config.DataDir())
			fmt.Fprintf(out, "advertise: %s\n", n.AdvertiseAddr())
			if generated {
				fmt.Fprintln(out)
				fmt.Fprintln(out, "cluster secret (copy to every other node via --secret):")
				fmt.Fprintln(out, "  "+secret)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&advertise, "advertise", "", "address peers should use to reach this node (host:port)")
	cmd.Flags().StringVar(&bindAddr, "bind", "0.0.0.0", "listen address for inter-node traffic")
	cmd.Flags().IntVar(&bindPort, "port", 9901, "listen port for inter-node traffic")
	cmd.Flags().StringVar(&clusterSecret, "secret", "", "shared cluster join secret (omit on the first node to auto-generate)")
	root.AddCommand(cmd)
}

// generateSecret produces 32 bytes of crypto-random data and returns
// it base64-encoded. Long enough that brute force isn't a concern;
// short enough that operators can copy-paste it without pagination.
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
