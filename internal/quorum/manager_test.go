package quorum

import (
	"testing"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

func threeNode(self string) (*config.ClusterConfig, *Manager) {
	cluster := &config.ClusterConfig{Peers: []config.PeerInfo{
		{NodeID: "a"}, {NodeID: "b"}, {NodeID: "c"},
	}}
	return cluster, New(self, cluster, nil)
}

func TestSoloNodeElectsItself(t *testing.T) {
	cluster := &config.ClusterConfig{}
	m := New("only", cluster, nil)
	m.markLive("only")
	m.recomputeMaster()
	if m.Master() != "only" {
		t.Errorf("Master=%q want %q", m.Master(), "only")
	}
	if !m.HasQuorum() {
		t.Error("solo node should have quorum")
	}
	if m.Term() != 1 {
		t.Errorf("Term=%d want 1 after first election", m.Term())
	}
}

func TestThreeNodeElectsLowestNodeID(t *testing.T) {
	_, m := threeNode("b")
	m.markLive("a")
	m.markLive("b")
	m.markLive("c")
	m.recomputeMaster()
	if got := m.Master(); got != "a" {
		t.Errorf("Master=%q want a", got)
	}
	if !m.HasQuorum() {
		t.Error("expected quorum with 3 live of 3")
	}
}

func TestNoQuorumClearsMaster(t *testing.T) {
	_, m := threeNode("b")
	m.markLive("b")
	m.recomputeMaster()
	if m.Master() != "" {
		t.Errorf("Master=%q want empty (no quorum)", m.Master())
	}
	if m.HasQuorum() {
		t.Error("1 of 3 live should not be quorum")
	}
}

func TestTermBumpsOnMasterChange(t *testing.T) {
	_, m := threeNode("b")
	m.markLive("a")
	m.markLive("b")
	m.recomputeMaster()
	termBefore := m.Term()
	masterBefore := m.Master()
	if masterBefore != "a" {
		t.Fatalf("expected initial master a, got %q", masterBefore)
	}

	// "a" goes dead — we and "c" join up.
	m.mu.Lock()
	delete(m.lastSeen, "a")
	m.mu.Unlock()
	m.markLive("c")
	m.recomputeMaster()
	if m.Master() != "b" {
		t.Errorf("after a-fail Master=%q want b", m.Master())
	}
	if m.Term() <= termBefore {
		t.Errorf("Term did not bump: before=%d after=%d", termBefore, m.Term())
	}
}

func TestHandleHeartbeatMarksSenderLive(t *testing.T) {
	cluster, m := threeNode("a")
	_ = cluster
	resp := m.HandleHeartbeat(transport.HeartbeatRequest{
		FromNodeID: "b",
		Term:       7,
		MasterID:   "a",
		Version:    3,
	})
	if resp.NodeID != "a" {
		t.Errorf("response NodeID=%q want a", resp.NodeID)
	}
	if _, ok := m.Liveness()["b"]; !ok {
		t.Error("sender was not recorded live")
	}
}

func TestDeadAfterEvictsStaleLiveness(t *testing.T) {
	_, m := threeNode("a")
	m.deadAfter = 50 * time.Millisecond
	m.markLive("a")
	m.markLive("b")
	m.markLive("c")
	m.recomputeMaster()
	if m.Master() != "a" {
		t.Fatal("expected initial master a")
	}

	// Wait past the dead-after window — only self remains live.
	time.Sleep(120 * time.Millisecond)
	m.markLive("a")
	m.recomputeMaster()
	if m.Master() != "" {
		t.Errorf("expected no master after peers timed out, got %q", m.Master())
	}
}

func TestVersionObserverFiresOnHigherVersion(t *testing.T) {
	cluster := &config.ClusterConfig{Version: 2}
	m := New("a", cluster, nil)

	var notified struct {
		peerID  string
		peerVer uint64
		count   int
	}
	m.SetVersionObserver(func(peerID, _ string, peerVer uint64) {
		notified.peerID = peerID
		notified.peerVer = peerVer
		notified.count++
	})

	// Seed the address for "b" via an incoming heartbeat — the
	// observer no-ops without one to avoid log spam.
	m.HandleHeartbeat(transport.HeartbeatRequest{
		FromNodeID: "b", Advertise: "10.0.0.2:9901", Version: 2,
	})

	m.maybeNotifyVersion("b", 5)
	if notified.count != 1 || notified.peerID != "b" || notified.peerVer != 5 {
		t.Errorf("expected observer fired with b=5, got %+v", notified)
	}

	m.maybeNotifyVersion("b", 1)
	if notified.count != 1 {
		t.Errorf("observer fired for stale version, count=%d", notified.count)
	}
}

func TestVersionObserverSkippedWithoutAddress(t *testing.T) {
	cluster := &config.ClusterConfig{Version: 0}
	m := New("a", cluster, nil)

	var fired int
	m.SetVersionObserver(func(_, _ string, _ uint64) { fired++ })

	// Peer "c" has never sent a heartbeat — no recorded address.
	m.maybeNotifyVersion("c", 99)
	if fired != 0 {
		t.Errorf("observer fired without a known address: %d", fired)
	}
}
