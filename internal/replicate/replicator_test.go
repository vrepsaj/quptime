package replicate

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

type fakeMaster struct {
	master    string
	isMaster  bool
	hasQuorum bool
}

func (f *fakeMaster) Master() string  { return f.master }
func (f *fakeMaster) IsMaster() bool  { return f.isMaster }
func (f *fakeMaster) HasQuorum() bool { return f.hasQuorum }

// stubClient records every Call without doing any actual I/O.
type stubClient struct {
	mu    sync.Mutex
	calls []string
}

func (s *stubClient) Call(_ context.Context, _, _, method string, _, _ any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, method)
	return nil
}

func newReplicator(t *testing.T, isMaster, hasQuorum bool) (*Replicator, *config.ClusterConfig, *stubClient) {
	t.Helper()
	t.Setenv("QUPTIME_DIR", t.TempDir())
	cluster := &config.ClusterConfig{}
	fm := &fakeMaster{master: "self", isMaster: isMaster, hasQuorum: hasQuorum}
	stub := &stubClient{}
	r := New("self", cluster, stub, fm)
	return r, cluster, stub
}

func TestApplyAddCheck(t *testing.T) {
	r, cluster, _ := newReplicator(t, true, true)
	payload, _ := json.Marshal(config.Check{ID: "c1", Name: "homepage", Type: config.CheckHTTP, Target: "https://example.com"})

	ver, err := r.LocalMutate(context.Background(), transport.MutationAddCheck, json.RawMessage(payload))
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Errorf("version=%d want 1", ver)
	}
	if len(cluster.Snapshot().Checks) != 1 {
		t.Errorf("expected 1 check, got %d", len(cluster.Snapshot().Checks))
	}
}

func TestApplyRemoveCheck(t *testing.T) {
	r, cluster, _ := newReplicator(t, true, true)
	_ = cluster.Mutate("self", func(c *config.ClusterConfig) error {
		c.Checks = []config.Check{{ID: "c1", Name: "x"}, {ID: "c2", Name: "y"}}
		return nil
	})

	target, _ := json.Marshal("x")
	ver, err := r.LocalMutate(context.Background(), transport.MutationRemoveCheck, json.RawMessage(target))
	if err != nil {
		t.Fatal(err)
	}
	if ver < 2 {
		t.Errorf("version did not advance: %d", ver)
	}
	cs := cluster.Snapshot().Checks
	if len(cs) != 1 || cs[0].ID != "c2" {
		t.Errorf("expected only c2 remaining, got %+v", cs)
	}
}

func TestApplyAddAndRemoveAlertAndPeer(t *testing.T) {
	r, cluster, _ := newReplicator(t, true, true)

	alert, _ := json.Marshal(config.Alert{ID: "a1", Name: "notify", Type: config.AlertDiscord})
	if _, err := r.LocalMutate(context.Background(), transport.MutationAddAlert, json.RawMessage(alert)); err != nil {
		t.Fatal(err)
	}

	peer, _ := json.Marshal(config.PeerInfo{NodeID: "p1", Advertise: "10.0.0.1:9901", Fingerprint: "fp"})
	if _, err := r.LocalMutate(context.Background(), transport.MutationAddPeer, json.RawMessage(peer)); err != nil {
		t.Fatal(err)
	}

	snap := cluster.Snapshot()
	if len(snap.Alerts) != 1 || len(snap.Peers) != 1 {
		t.Fatalf("missing entries: %+v", snap)
	}

	target, _ := json.Marshal("notify")
	if _, err := r.LocalMutate(context.Background(), transport.MutationRemoveAlert, json.RawMessage(target)); err != nil {
		t.Fatal(err)
	}
	target, _ = json.Marshal("p1")
	if _, err := r.LocalMutate(context.Background(), transport.MutationRemovePeer, json.RawMessage(target)); err != nil {
		t.Fatal(err)
	}

	snap = cluster.Snapshot()
	if len(snap.Alerts) != 0 || len(snap.Peers) != 0 {
		t.Errorf("entries not removed: %+v", snap)
	}
}

func TestMutateRequiresQuorum(t *testing.T) {
	r, _, _ := newReplicator(t, true, false)
	_, err := r.LocalMutate(context.Background(), transport.MutationAddCheck, json.RawMessage("{}"))
	if err == nil {
		t.Error("expected quorum-required error")
	}
}

func TestHandleApplyClusterCfgGatesOnVersion(t *testing.T) {
	r, cluster, _ := newReplicator(t, false, true)
	// Push local version to 7 directly via Replace (Mutate would
	// implicitly bump to 8 and confuse the test cases below).
	if _, err := cluster.Replace(&config.ClusterConfig{Version: 7}); err != nil {
		t.Fatal(err)
	}

	if resp := r.HandleApplyClusterCfg(transport.ApplyClusterCfgRequest{
		Config: &config.ClusterConfig{Version: 6},
	}); resp.Applied {
		t.Error("older snapshot was applied")
	}
	if resp := r.HandleApplyClusterCfg(transport.ApplyClusterCfgRequest{
		Config: &config.ClusterConfig{Version: 7},
	}); resp.Applied {
		t.Error("same-version snapshot was applied")
	}

	resp := r.HandleApplyClusterCfg(transport.ApplyClusterCfgRequest{
		Config: &config.ClusterConfig{Version: 8, Checks: []config.Check{{ID: "n"}}},
	})
	if !resp.Applied {
		t.Error("newer snapshot was rejected")
	}
	if cluster.Snapshot().Version != 8 {
		t.Errorf("local version did not advance: %d", cluster.Snapshot().Version)
	}
}

// TestEnrollmentLifecycle walks the full enrollment-token mutation
// path: master records an enrollment, records the joiner's pending
// material under it, then approves — which atomically removes the
// token entry and installs the joiner as a peer.
func TestEnrollmentLifecycle(t *testing.T) {
	r, cluster, _ := newReplicator(t, true, true)

	// 1. Add the enrollment.
	enrollPayload, _ := json.Marshal(config.PendingEnrollment{
		ID:         "tok-abc",
		Name:       "bravo",
		SecretHash: "sha256:deadbeef",
	})
	if _, err := r.LocalMutate(context.Background(), transport.MutationAddEnrollment, json.RawMessage(enrollPayload)); err != nil {
		t.Fatal(err)
	}
	if got := len(cluster.Snapshot().PendingEnrollments); got != 1 {
		t.Fatalf("after add: enrollments=%d want 1", got)
	}

	// 2. Record a joiner under it.
	recordPayload, _ := json.Marshal(struct {
		ID          string              `json:"id"`
		PendingJoin *config.PendingJoin `json:"pending_join"`
	}{
		ID: "tok-abc",
		PendingJoin: &config.PendingJoin{
			NodeID:      "joiner-1",
			Advertise:   "joiner.example.com:9901",
			Fingerprint: "sha256:cafef00d",
			CertPEM:     "-----CERT-----",
		},
	})
	if _, err := r.LocalMutate(context.Background(), transport.MutationRecordEnrollPending, json.RawMessage(recordPayload)); err != nil {
		t.Fatal(err)
	}
	got := cluster.Snapshot().PendingEnrollments[0]
	if got.PendingJoin == nil || got.PendingJoin.NodeID != "joiner-1" {
		t.Fatalf("pending join not recorded: %+v", got)
	}

	// 3. Approve.
	approve, _ := json.Marshal("tok-abc")
	if _, err := r.LocalMutate(context.Background(), transport.MutationApproveEnrollment, json.RawMessage(approve)); err != nil {
		t.Fatal(err)
	}
	snap := cluster.Snapshot()
	if len(snap.PendingEnrollments) != 0 {
		t.Errorf("approval did not clear token: %+v", snap.PendingEnrollments)
	}
	if len(snap.Peers) != 1 || snap.Peers[0].NodeID != "joiner-1" {
		t.Errorf("approval did not add peer: %+v", snap.Peers)
	}
}

// TestApproveEnrollmentWithoutPendingJoinFails — approval on a token
// that has not been claimed yet must return an error rather than
// silently committing a half-formed peer entry.
func TestApproveEnrollmentWithoutPendingJoinFails(t *testing.T) {
	r, cluster, _ := newReplicator(t, true, true)
	enrollPayload, _ := json.Marshal(config.PendingEnrollment{
		ID:         "tok-xyz",
		SecretHash: "sha256:abcd",
	})
	if _, err := r.LocalMutate(context.Background(), transport.MutationAddEnrollment, json.RawMessage(enrollPayload)); err != nil {
		t.Fatal(err)
	}

	target, _ := json.Marshal("tok-xyz")
	if _, err := r.LocalMutate(context.Background(), transport.MutationApproveEnrollment, json.RawMessage(target)); err == nil {
		t.Fatal("expected approve-without-pending to fail")
	}
	if len(cluster.Snapshot().Peers) != 0 {
		t.Errorf("approve-without-pending added a peer anyway: %+v", cluster.Snapshot().Peers)
	}
}

// TestRevokeEnrollment drops a token from cluster.yaml without
// touching peers.
func TestRevokeEnrollment(t *testing.T) {
	r, cluster, _ := newReplicator(t, true, true)
	enrollPayload, _ := json.Marshal(config.PendingEnrollment{
		ID:         "tok-rev",
		SecretHash: "sha256:abcd",
	})
	if _, err := r.LocalMutate(context.Background(), transport.MutationAddEnrollment, json.RawMessage(enrollPayload)); err != nil {
		t.Fatal(err)
	}

	target, _ := json.Marshal("tok-rev")
	if _, err := r.LocalMutate(context.Background(), transport.MutationRemoveEnrollment, json.RawMessage(target)); err != nil {
		t.Fatal(err)
	}
	if got := len(cluster.Snapshot().PendingEnrollments); got != 0 {
		t.Errorf("revoke left %d enrollment(s)", got)
	}
}

func TestHandleProposeMutationRejectsNonMaster(t *testing.T) {
	r, _, _ := newReplicator(t, false, true)
	resp := r.HandleProposeMutation(context.Background(), transport.ProposeMutationRequest{
		FromNodeID: "follower",
		Kind:       transport.MutationAddCheck,
		Payload:    json.RawMessage(`{}`),
	})
	if resp.Error == "" {
		t.Error("follower accepted a proposal")
	}
}
