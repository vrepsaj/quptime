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
	MethodEnroll          = "Enroll"
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

// JoinRequest is the legacy bootstrap request. Kept declared only so
// pre-v0.2.0 binaries that still send this message can finish their
// framing — the new daemon's MethodJoin handler ignores the payload
// entirely and always returns a deprecation message pointing at the
// enrollment-token flow.
//
// Deprecated: use EnrollRequest. Will be removed once the installed
// base has rotated past v0.2.0.
type JoinRequest struct {
	NodeID        string `json:"node_id"`
	Advertise     string `json:"advertise"`
	Fingerprint   string `json:"fingerprint"`
	CertPEM       string `json:"cert_pem"`
	ClusterSecret string `json:"cluster_secret"`
}

// JoinResponse is what the deprecation stub returns to a legacy peer
// that tries the old Join flow. Error always carries a pointer at
// `qu enroll`; Accepted is always false.
type JoinResponse struct {
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

// EnrollRequest is sent by a new node presenting a pre-deployment
// enrollment token. It replaces the cluster-secret Join flow: the
// cluster operator generated TokenID + TokenSecret out-of-band, the
// joiner presents them along with its full identity (NodeID, cert,
// fingerprint), and the cluster authorizes membership.
//
// The TLS connection itself is authenticated by the joiner verifying
// the cluster's leaf cert against the fingerprints baked into the
// token *before* sending this request — so the joiner already knows
// it is talking to the right cluster. The token-secret check here
// authenticates the joiner to the cluster.
type EnrollRequest struct {
	TokenID     string `json:"token_id"`
	TokenSecret string `json:"token_secret"`

	NodeID      string `json:"node_id"`
	Advertise   string `json:"advertise"`
	Fingerprint string `json:"fingerprint"`
	CertPEM     string `json:"cert_pem"`
}

// EnrollResponse is the cluster's reply to an enrollment attempt.
//
//   - Accepted=true  → joiner is now a peer. Cluster snapshot (with
//     every existing peer's cert) is included so the
//     joiner can populate its own trust store before
//     starting `qu serve`.
//   - Pending=true   → token valid but AutoApprove=false; the joiner's
//     identity has been recorded. An operator on the
//     cluster must run `qu enroll approve <id>` to
//     finalize. Cluster (the snapshot) is not
//     returned in this case — replication only kicks
//     in once the joiner is a real peer.
//   - Error          → the token was invalid (unknown, expired, secret
//     mismatch) or the request itself was malformed.
type EnrollResponse struct {
	Accepted bool                  `json:"accepted"`
	Pending  bool                  `json:"pending,omitempty"`
	Error    string                `json:"error,omitempty"`
	Cluster  *enrollClusterSummary `json:"cluster,omitempty"`
}

// enrollClusterSummary carries the minimum the joiner needs to trust
// every existing peer post-acceptance: NodeID, advertise, and cert
// PEM (fingerprint is derived from the cert).
//
// Defined as its own type rather than reusing config.PeerInfo to
// avoid the transport package importing config (which currently
// flows the other direction) and to make the wire shape explicit.
type enrollClusterSummary struct {
	Peers []EnrolledPeer `json:"peers"`
}

// EnrolledPeer is one peer's public material as shipped back to a
// successful enrollee.
type EnrolledPeer struct {
	NodeID      string `json:"node_id"`
	Advertise   string `json:"advertise"`
	Fingerprint string `json:"fingerprint"`
	CertPEM     string `json:"cert_pem"`
}

// NewEnrollSummary builds the success-response payload from a slice of
// peers. Exported so the daemon can construct it from config.PeerInfo
// without the transport package depending on the config package.
func NewEnrollSummary(peers []EnrolledPeer) *enrollClusterSummary {
	return &enrollClusterSummary{Peers: peers}
}

// EnrollSummaryPeers returns the peers from an enroll response, or nil
// if the summary is nil. Helper to keep the unexported field accessible
// without exposing the wrapping struct.
func EnrollSummaryPeers(s *enrollClusterSummary) []EnrolledPeer {
	if s == nil {
		return nil
	}
	return s.Peers
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
	// MutationReplaceConfig overwrites the editable portions
	// (peers/checks/alerts) of cluster.yaml in one shot. The replicated
	// version, updated_at, and updated_by are still set by the master.
	// Used by the manual-edit watcher: an operator edits cluster.yaml
	// directly, the daemon detects it, and forwards the parsed snapshot
	// to the master through this mutation.
	MutationReplaceConfig MutationKind = "replace_config"

	// Enrollment-token lifecycle. All four are master-only and route
	// through the standard ProposeMutation path.
	MutationAddEnrollment       MutationKind = "add_enrollment"
	MutationRemoveEnrollment    MutationKind = "remove_enrollment"
	MutationRecordEnrollPending MutationKind = "record_enroll_pending"
	MutationApproveEnrollment   MutationKind = "approve_enrollment"
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
	// Alerts holds one display string per effective alert. Names of
	// default-attached alerts are suffixed with "*" so the operator can
	// see which fired without lookup.
	Alerts []string `json:"alerts,omitempty"`
}
