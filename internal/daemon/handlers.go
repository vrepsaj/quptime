package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"time"

	"git.cer.sh/axodouble/quptime/internal/checks"
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

	d.server.Handle(transport.MethodJoin, func(_ context.Context, _ string, raw json.RawMessage) (any, error) {
		var req transport.JoinRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return transport.JoinResponse{Error: err.Error()}, nil
		}
		// Constant-time secret check: every node in the cluster must
		// present the same shared secret. This is the only barrier
		// stopping a stranger who can reach :9901 from enrolling
		// themselves with their own fresh key.
		if subtle.ConstantTimeCompare([]byte(req.ClusterSecret), []byte(d.node.ClusterSecret)) != 1 {
			return transport.JoinResponse{Error: "cluster secret mismatch"}, nil
		}
		fp, err := crypto.FingerprintFromCertPEM([]byte(req.CertPEM))
		if err != nil {
			return transport.JoinResponse{Error: "parse cert: " + err.Error()}, nil
		}
		if fp != req.Fingerprint {
			return transport.JoinResponse{Error: "fingerprint mismatch"}, nil
		}
		if err := d.trust.Add(trust.Entry{
			NodeID:      req.NodeID,
			Address:     req.Advertise,
			Fingerprint: req.Fingerprint,
			CertPEM:     req.CertPEM,
		}); err != nil {
			return transport.JoinResponse{Error: err.Error()}, nil
		}
		return transport.JoinResponse{Accepted: true}, nil
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
		cs := transport.CheckSnapshot{CheckID: c.ID, Name: c.Name, State: "unknown"}
		if agg, ok := d.aggregator.SnapshotFor(c.ID); ok {
			cs.State = string(agg.State)
			cs.OKCount = agg.OKCount
			cs.Total = agg.Reports
			cs.Detail = agg.Detail
		}
		out.Checks = append(out.Checks, cs)
	}
	return out
}
