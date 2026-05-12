package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// NodeConfig is the per-node, never-replicated identity file.
type NodeConfig struct {
	// NodeID is a stable UUID generated at `qu init`. Used by all peers
	// to refer to this node across restarts and IP changes.
	NodeID string `yaml:"node_id"`

	// BindAddr is the address the daemon listens on for inter-node
	// traffic. Defaults to 0.0.0.0.
	BindAddr string `yaml:"bind_addr"`

	// BindPort is the port the daemon listens on. Default 9901.
	BindPort int `yaml:"bind_port"`

	// Advertise is the address other nodes use to reach us. May differ
	// from BindAddr when behind NAT. Set explicitly via `qu init --advertise`.
	Advertise string `yaml:"advertise"`

	// ClusterSecret is the pre-shared secret every node in the cluster
	// must present during the Join RPC. Without it any operator who
	// can reach :9901 could enrol themselves into the cluster, so we
	// require an out-of-band copy at `qu init` time. Stored locally
	// only, never replicated.
	ClusterSecret string `yaml:"cluster_secret"`
}

// AdvertiseAddr returns the address peers should dial. Falls back to
// BindAddr:BindPort if Advertise is empty.
func (n *NodeConfig) AdvertiseAddr() string {
	if n.Advertise != "" {
		return n.Advertise
	}
	bind := n.BindAddr
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		bind = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", bind, n.BindPort)
}

// LoadNodeConfig reads node.yaml from the data dir.
func LoadNodeConfig() (*NodeConfig, error) {
	raw, err := os.ReadFile(NodeFilePath())
	if err != nil {
		return nil, err
	}
	cfg := &NodeConfig{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse node.yaml: %w", err)
	}
	if cfg.BindPort == 0 {
		cfg.BindPort = 9901
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = "0.0.0.0"
	}
	return cfg, nil
}

// Save writes node.yaml atomically.
func (n *NodeConfig) Save() error {
	out, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return AtomicWrite(NodeFilePath(), out, 0o600)
}
