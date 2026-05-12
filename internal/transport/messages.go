// Package transport carries inter-node RPC over mTLS. It owns three
// concerns and nothing else:
//
//  1. Building tls.Config values that pin peer certs against the local
//     trust store (server and client side).
//  2. Length-prefixed JSON framing on top of the TLS connection.
//  3. A tiny method-dispatch RPC: callers register handlers by method
//     name; remote peers invoke them via Client.Call.
//
// Higher-level concerns (heartbeats, quorum, replication, check
// shipping) live in their own packages and use this one purely as a
// pipe. That keeps the wire format easy to reason about and the
// surrounding packages testable without a real network.
package transport

import (
	"encoding/json"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// Method names. Defined here so every package agrees on the wire-level
// identifier without importing each other.
const (
	MethodPing            = "Ping"
	MethodWhoAmI          = "WhoAmI"
	MethodJoin            = "Join"
	MethodHeartbeat       = "Heartbeat"
	MethodGetClusterCfg   = "GetClusterCfg"
	MethodApplyClusterCfg = "ApplyClusterCfg"
	MethodProposeMutation = "ProposeMutation"
	MethodReportResult    = "ReportResult"
	MethodStatus          = "Status"
)

// PingRequest is an empty liveness probe. PingResponse carries the
// responder's wall clock so the caller can sanity-check drift.
type PingRequest struct{}

// PingResponse is returned by MethodPing.
type PingResponse struct {
	NodeID string    `json:"node_id"`
	Now    time.Time `json:"now"`
}

// WhoAmIRequest asks the remote node to identify itself. Used during
// the TOFU handshake before the caller commits a trust entry.
type WhoAmIRequest struct{}

// WhoAmIResponse carries the node's identity. The fingerprint is
// recomputed by the caller from the TLS cert and compared against the
// claim here as a defense-in-depth check.
type WhoAmIResponse struct {
	NodeID      string `json:"node_id"`
	Advertise   string `json:"advertise"`
	Fingerprint string `json:"fingerprint"`
	CertPEM     string `json:"cert_pem"`
}

// JoinRequest is sent by a node that has just learned the remote's
// fingerprint out of band and wants the remote to record this node in
// its own trust store too (so the relationship is symmetric).
//
// ClusterSecret is the pre-shared cluster join key. The recipient
// rejects the request unless it matches the locally-configured secret
// in constant time.
type JoinRequest struct {
	NodeID        string `json:"node_id"`
	Advertise     string `json:"advertise"`
	Fingerprint   string `json:"fingerprint"`
	CertPEM       string `json:"cert_pem"`
	ClusterSecret string `json:"cluster_secret"`
}

// JoinResponse echoes a non-empty Error string when the remote refuses
// the join (e.g. operator declined the prompt or fingerprint mismatch).
type JoinResponse struct {
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

// HeartbeatRequest is the periodic liveness ping sent over the
// inter-node channel. It also carries the sender's view of who the
// master is, so disagreements surface quickly. Advertise lets the
// recipient cache where to reach the sender, which matters when the
// sender isn't yet in our cluster.yaml peers list (e.g. mid-bootstrap).
type HeartbeatRequest struct {
	FromNodeID string `json:"from_node_id"`
	Advertise  string `json:"advertise"`
	Term       uint64 `json:"term"`
	MasterID   string `json:"master_id"`
	Version    uint64 `json:"config_version"`
}

// HeartbeatResponse is returned by MethodHeartbeat.
type HeartbeatResponse struct {
	NodeID    string `json:"node_id"`
	Advertise string `json:"advertise"`
	Term      uint64 `json:"term"`
	MasterID  string `json:"master_id"`
	Version   uint64 `json:"config_version"`
}

// GetClusterCfgRequest fetches the responder's view of cluster.yaml.
// Used by stale followers to pull the canonical config from master.
type GetClusterCfgRequest struct{}

// GetClusterCfgResponse contains a cluster.yaml snapshot.
type GetClusterCfgResponse struct {
	Config *config.ClusterConfig `json:"config"`
}

// ApplyClusterCfgRequest is the master pushing a new replicated config
// to a follower. The follower applies only if Version is strictly
// greater than its local Version.
type ApplyClusterCfgRequest struct {
	Config *config.ClusterConfig `json:"config"`
}

// ApplyClusterCfgResponse acknowledges with whether the follower
// stored the new config.
type ApplyClusterCfgResponse struct {
	Applied bool   `json:"applied"`
	Version uint64 `json:"current_version"`
}

// MutationKind enumerates the cluster-config edit operations that
// followers forward to the master.
type MutationKind string

const (
	MutationAddCheck    MutationKind = "add_check"
	MutationRemoveCheck MutationKind = "remove_check"
	MutationAddAlert    MutationKind = "add_alert"
	MutationRemoveAlert MutationKind = "remove_alert"
	MutationAddPeer     MutationKind = "add_peer"
	MutationRemovePeer  MutationKind = "remove_peer"
)

// ProposeMutationRequest is a follower-to-master message. The payload
// is the JSON-encoded body of the new entity (a Check, an Alert, or a
// PeerInfo) for the "add" variants, or the target ID/NodeID string for
// removals.
type ProposeMutationRequest struct {
	FromNodeID string          `json:"from_node_id"`
	Kind       MutationKind    `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
}

// ProposeMutationResponse is the master's reply to ProposeMutation.
type ProposeMutationResponse struct {
	NewVersion uint64 `json:"new_version"`
	Error      string `json:"error,omitempty"`
}

// ReportResultRequest is a follower-to-master message reporting the
// outcome of a single local probe.
type ReportResultRequest struct {
	FromNodeID string    `json:"from_node_id"`
	CheckID    string    `json:"check_id"`
	OK         bool      `json:"ok"`
	Detail     string    `json:"detail,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	At         time.Time `json:"at"`
}

// ReportResultResponse acknowledges a result. Empty body for now.
type ReportResultResponse struct{}

// StatusRequest asks a peer for its operational state.
type StatusRequest struct{}

// StatusResponse is what `qu status` aggregates and displays.
type StatusResponse struct {
	NodeID     string          `json:"node_id"`
	Term       uint64          `json:"term"`
	MasterID   string          `json:"master_id"`
	Version    uint64          `json:"config_version"`
	Peers      []PeerLiveness  `json:"peers"`
	Checks     []CheckSnapshot `json:"checks"`
	HasQuorum  bool            `json:"has_quorum"`
	QuorumSize int             `json:"quorum_size"`
}

// PeerLiveness summarises one peer for status output.
type PeerLiveness struct {
	NodeID    string    `json:"node_id"`
	Advertise string    `json:"advertise"`
	Live      bool      `json:"live"`
	LastSeen  time.Time `json:"last_seen"`
}

// CheckSnapshot is the aggregate state of one configured check.
type CheckSnapshot struct {
	CheckID string `json:"check_id"`
	Name    string `json:"name"`
	State   string `json:"state"` // "up", "down", "unknown"
	OKCount int    `json:"ok_count"`
	Total   int    `json:"total"`
	Detail  string `json:"detail,omitempty"`
}
