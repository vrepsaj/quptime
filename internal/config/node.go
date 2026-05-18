package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Environment variable names that override fields on NodeConfig at
// load time. Intended to let `docker compose` setups drive a node's
// identity and listener configuration without having to bake a
// node.yaml into the image or run `qu init` manually first.
//
// Empty values are ignored — they do not clear a field. The override
// order is therefore: env (non-empty) > file > compiled default.
const (
	EnvNodeID    = "QUPTIME_NODE_ID"
	EnvBindAddr  = "QUPTIME_BIND_ADDR"
	EnvBindPort  = "QUPTIME_BIND_PORT"
	EnvAdvertise = "QUPTIME_ADVERTISE"

	// EnvClusterSecret remains declared so existing docker/compose
	// setups don't fail their env-validation steps after an upgrade,
	// but the daemon no longer reads it. New nodes enroll via
	// `qu enroll create` tokens (see internal/daemon/handlers.go).
	EnvClusterSecret = "QUPTIME_CLUSTER_SECRET"
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

	// ClusterSecret is the legacy pre-shared cluster join key. The
	// daemon no longer reads or validates it — pre-deployment
	// enrollment tokens (see internal/daemon/handlers.go) replaced
	// the single-secret model. The field is kept so old node.yaml
	// files continue to parse; the daemon blanks it on first start
	// after an upgrade (see daemon.Daemon.New).
	//
	// Deprecated: set up new nodes with `qu enroll create` instead.
	ClusterSecret string `yaml:"cluster_secret,omitempty"`
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

// ApplyEnvOverrides folds QUPTIME_* environment variables onto n.
// Non-empty env values win over the existing field value. Called both
// by LoadNodeConfig and by the `qu init` / serve auto-init paths so
// the same precedence rules apply whether the daemon is reading a
// persisted node.yaml or constructing one from scratch.
func (n *NodeConfig) ApplyEnvOverrides() error {
	if v := os.Getenv(EnvNodeID); v != "" {
		n.NodeID = v
	}
	if v := os.Getenv(EnvBindAddr); v != "" {
		n.BindAddr = v
	}
	if v := os.Getenv(EnvBindPort); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s=%q: not an integer: %w", EnvBindPort, v, err)
		}
		n.BindPort = p
	}
	if v := os.Getenv(EnvAdvertise); v != "" {
		n.Advertise = v
	}
	// QUPTIME_CLUSTER_SECRET is silently ignored: enrollment tokens
	// have replaced the shared-secret join model. We do not surface
	// an error for it because docker-compose files in the wild may
	// still set it, and we'd rather not break their bring-up.
	return nil
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
	if err := cfg.ApplyEnvOverrides(); err != nil {
		return nil, err
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
