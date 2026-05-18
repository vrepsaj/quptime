package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"git.cer.sh/axodouble/quptime/internal/checks"
	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/crypto"
	"git.cer.sh/axodouble/quptime/internal/transport"
	"git.cer.sh/axodouble/quptime/internal/trust"
)

// registerHandlers wires every inter-node RPC method that the daemon
// understands onto the transport server. Each method delegates to the
// owning subsystem (quorum, replicator, etc.) so this file stays a
// thin dispatch table.
func (d *Daemon) registerHandlers() {
	d.server.Handle(transport.MethodPing, func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		return transport.PingResponse{NodeID: d.node.NodeID, Now: time.Now().UTC()}, nil
	})

	d.server.Handle(transport.MethodWhoAmI, func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		fp, err := crypto.FingerprintFromCertPEM(d.assets.Cert)
		if err != nil {
			return nil, err
		}
		return transport.WhoAmIResponse{
			NodeID:      d.node.NodeID,
			Advertise:   d.node.AdvertiseAddr(),
			Fingerprint: fp,
			CertPEM:     string(d.assets.Cert),
		}, nil
	})

	d.server.Handle(transport.MethodJoin, func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		// The shared-cluster-secret Join flow is removed: it was the
		// single point of compromise the new enrollment-token model
		// was introduced to eliminate. We keep the method registered
		// so a node running an older binary gets an actionable error
		// rather than a TLS-layer reject — the operator should switch
		// to `qu enroll create` on the cluster and `qu enroll join
		// <token>` on the new host.
		return transport.JoinResponse{
			Error: "cluster-secret join is no longer supported — issue a pre-deployment token with `qu enroll create` and run `qu enroll join <token>` on the new host",
		}, nil
	})

	d.server.Handle(transport.MethodEnroll, func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
		var req transport.EnrollRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return transport.EnrollResponse{Error: "decode: " + err.Error()}, nil
		}
		return d.handleEnroll(ctx, req), nil
	})

	d.server.Handle(transport.MethodHeartbeat, func(_ context.Context, _ string, raw json.RawMessage) (any, error) {
		var req transport.HeartbeatRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return d.quorum.HandleHeartbeat(req), nil
	})

	d.server.Handle(transport.MethodGetClusterCfg, func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		return d.replicator.HandleGetClusterCfg(), nil
	})

	d.server.Handle(transport.MethodApplyClusterCfg, func(_ context.Context, _ string, raw json.RawMessage) (any, error) {
		var req transport.ApplyClusterCfgRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return d.replicator.HandleApplyClusterCfg(req), nil
	})

	d.server.Handle(transport.MethodProposeMutation, func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
		var req transport.ProposeMutationRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return d.replicator.HandleProposeMutation(ctx, req), nil
	})

	d.server.Handle(transport.MethodReportResult, func(_ context.Context, _ string, raw json.RawMessage) (any, error) {
		var req transport.ReportResultRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		res := checks.Result{
			CheckID:   req.CheckID,
			OK:        req.OK,
			Detail:    req.Detail,
			Latency:   time.Duration(req.LatencyMS) * time.Millisecond,
			Timestamp: req.At,
		}
		d.aggregator.Submit(req.FromNodeID, res)
		return transport.ReportResultResponse{}, nil
	})

	d.server.Handle(transport.MethodStatus, func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		return d.buildStatus(), nil
	})
}

// handleEnroll is the inter-node-RPC implementation of MethodEnroll.
//
// Either side of the cluster (master or follower) can serve the call
// because the underlying cluster-state mutations route through the
// replicator, which forwards to master when needed. Validation
// (token-secret hash compare, expiry, cert-fingerprint consistency)
// happens locally against the replicated cluster.yaml; both sides see
// the same set of pending enrollments.
func (d *Daemon) handleEnroll(ctx context.Context, req transport.EnrollRequest) transport.EnrollResponse {
	if req.TokenID == "" || req.TokenSecret == "" {
		return transport.EnrollResponse{Error: "token_id and token_secret are required"}
	}
	if req.NodeID == "" || req.CertPEM == "" || req.Fingerprint == "" {
		return transport.EnrollResponse{Error: "node_id, cert_pem and fingerprint are required"}
	}

	// Sanity: the fingerprint the joiner claims must match the cert
	// they actually sent. This is defense in depth — the TLS handshake
	// itself already established what cert was presented at the
	// connection level, but this RPC arrives in the request body so
	// we double-check the two agree.
	derivedFP, err := crypto.FingerprintFromCertPEM([]byte(req.CertPEM))
	if err != nil {
		return transport.EnrollResponse{Error: "parse cert: " + err.Error()}
	}
	if derivedFP != req.Fingerprint {
		return transport.EnrollResponse{Error: "claimed fingerprint does not match supplied cert"}
	}

	entry := d.cluster.FindEnrollmentByID(req.TokenID)
	if entry == nil {
		return transport.EnrollResponse{Error: "unknown enrollment token"}
	}
	if !entry.ExpiresAt.IsZero() && time.Now().After(entry.ExpiresAt) {
		return transport.EnrollResponse{Error: "enrollment token expired"}
	}
	if !verifyEnrollSecret(req.TokenSecret, entry.SecretHash) {
		return transport.EnrollResponse{Error: "invalid enrollment token"}
	}

	pj := &config.PendingJoin{
		NodeID:      req.NodeID,
		Advertise:   req.Advertise,
		Fingerprint: req.Fingerprint,
		CertPEM:     req.CertPEM,
		SubmittedAt: time.Now().UTC(),
	}

	// Record the joiner under the token. For auto-approve tokens this
	// is a transient step before the immediate ApproveEnrollment; for
	// manual tokens it parks the joiner waiting for `qu enroll approve`.
	recordPayload := struct {
		ID          string              `json:"id"`
		PendingJoin *config.PendingJoin `json:"pending_join"`
	}{ID: entry.ID, PendingJoin: pj}
	if _, err := d.replicator.LocalMutate(ctx, transport.MutationRecordEnrollPending, recordPayload); err != nil {
		return transport.EnrollResponse{Error: "record pending join: " + err.Error()}
	}

	if !entry.AutoApprove {
		return transport.EnrollResponse{Pending: true}
	}

	// Auto-approve: collapse the token + the just-recorded pending
	// join into a peer entry in cluster.yaml. The cluster-config
	// OnChange callback (daemon.syncTrustFromCluster) will pick up
	// the new peer and add it to this node's trust store; the master
	// broadcast will do the same on every other peer.
	if _, err := d.replicator.LocalMutate(ctx, transport.MutationApproveEnrollment, entry.ID); err != nil {
		return transport.EnrollResponse{Error: "auto-approve: " + err.Error()}
	}

	// Eager local trust: don't wait for the OnChange callback to
	// finish — the joiner is going to immediately try to mTLS back to
	// us for cluster catch-up, so make sure we accept its cert right
	// now. Add idempotently.
	if err := d.trust.Add(trust.Entry{
		NodeID:      pj.NodeID,
		Address:     pj.Advertise,
		Fingerprint: pj.Fingerprint,
		CertPEM:     pj.CertPEM,
	}); err != nil {
		d.logger.Printf("enroll: trust add for %s: %v", pj.NodeID, err)
	}

	return transport.EnrollResponse{
		Accepted: true,
		Cluster:  transport.NewEnrollSummary(d.enrollPeersForJoiner(pj.NodeID)),
	}
}

// enrollPeersForJoiner builds the peer summary returned to a
// successful enrollee. It omits the joiner themselves (they obviously
// don't need their own cert in their trust store) and any peer that
// lacks cert material (which would prevent the joiner from trusting
// it anyway).
func (d *Daemon) enrollPeersForJoiner(joinerID string) []transport.EnrolledPeer {
	snap := d.cluster.Snapshot()
	out := make([]transport.EnrolledPeer, 0, len(snap.Peers))
	for _, p := range snap.Peers {
		if p.NodeID == joinerID || p.NodeID == "" {
			continue
		}
		if p.Fingerprint == "" || p.CertPEM == "" {
			continue
		}
		out = append(out, transport.EnrolledPeer{
			NodeID:      p.NodeID,
			Advertise:   p.Advertise,
			Fingerprint: p.Fingerprint,
			CertPEM:     p.CertPEM,
		})
	}
	return out
}

// hashEnrollSecret is the canonical secret-hash for storage in
// cluster.yaml. Plain sha256-hex — operators don't need to wait
// seconds for a single token check, and the secret itself is 32 bytes
// of crypto-random material that survives a brute-force attempt
// against any practical hash.
func hashEnrollSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// verifyEnrollSecret constant-time compares the presented secret
// against the stored hash.
func verifyEnrollSecret(presented, storedHash string) bool {
	want, err := decodeHashHex(storedHash)
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(want, got[:]) == 1
}

func decodeHashHex(stored string) ([]byte, error) {
	const prefix = "sha256:"
	if len(stored) <= len(prefix) || stored[:len(prefix)] != prefix {
		return nil, errors.New("hash has no sha256: prefix")
	}
	return hex.DecodeString(stored[len(prefix):])
}

// buildStatusForCLI is what the local control plane returns to `qu
// status`. Peers, quorum, and term come from the local view; check
// state is fetched from the master because the aggregator only runs
// there. If the master is unknown or unreachable, falls back to the
// local view (which will show "unknown" for every check).
func (d *Daemon) buildStatusForCLI(ctx context.Context) transport.StatusResponse {
	local := d.buildStatus()
	if d.quorum.IsMaster() {
		return local
	}
	masterID := d.quorum.Master()
	if masterID == "" {
		return local
	}
	addr := d.addressOf(masterID)
	if addr == "" {
		return local
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var remote transport.StatusResponse
	if err := d.client.Call(callCtx, masterID, addr, transport.MethodStatus, transport.StatusRequest{}, &remote); err != nil {
		d.logger.Printf("status: fetch from master %s: %v", masterID, err)
		return local
	}
	local.Checks = remote.Checks
	return local
}

// buildStatus is shared by both the inter-node Status RPC handler and
// the local control plane's "status" command.
func (d *Daemon) buildStatus() transport.StatusResponse {
	snap := d.cluster.Snapshot()
	liveness := d.quorum.Liveness()
	live := map[string]bool{}
	for _, id := range d.quorum.LiveSet() {
		live[id] = true
	}

	out := transport.StatusResponse{
		NodeID:     d.node.NodeID,
		Term:       d.quorum.Term(),
		MasterID:   d.quorum.Master(),
		Version:    snap.Version,
		HasQuorum:  d.quorum.HasQuorum(),
		QuorumSize: snap.QuorumSize(),
	}
	for _, p := range snap.Peers {
		out.Peers = append(out.Peers, transport.PeerLiveness{
			NodeID:    p.NodeID,
			Advertise: p.Advertise,
			Live:      live[p.NodeID],
			LastSeen:  liveness[p.NodeID],
		})
	}
	for _, c := range snap.Checks {
		check := c
		cs := transport.CheckSnapshot{CheckID: c.ID, Name: c.Name, State: "unknown"}
		if agg, ok := d.aggregator.SnapshotFor(c.ID); ok {
			cs.State = string(agg.State)
			cs.OKCount = agg.OKCount
			cs.Total = agg.Reports
			cs.Detail = agg.Detail
		}
		for _, a := range d.cluster.EffectiveAlertsFor(&check) {
			label := a.Name
			if a.Default {
				label += "*"
			}
			cs.Alerts = append(cs.Alerts, label)
		}
		out.Checks = append(out.Checks, cs)
	}
	return out
}
