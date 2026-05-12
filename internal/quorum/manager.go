// Package quorum owns membership liveness and master election.
//
// Model
//
//   - Membership N is the set of peers listed in cluster.yaml (every
//     node, including self).
//   - A peer is "live" if we have seen a heartbeat (sent or received)
//     within the dead-after window.
//   - Quorum is met when the live set's size is ≥ ⌈N/2⌉+1.
//   - When quorum holds, the master is the live member with the
//     lexicographically smallest NodeID. Otherwise the cluster has no
//     master.
//   - The term integer is bumped every time the elected master
//     changes — including transitions to and from "no master".
//
// The rule is deliberately deterministic: every node that sees the
// same live set picks the same master, so there is no negotiation
// step and no split-brain window.
package quorum

import (
	"context"
	"sort"
	"sync"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

// Defaults for the heartbeat loop. The dead-after is comfortably
// above three missed beats so a transient blip never trips a master
// re-election.
const (
	DefaultHeartbeatInterval = 1 * time.Second
	DefaultDeadAfter         = 4 * time.Second
)

// VersionObserver is invoked whenever a heartbeat exchange reveals
// that a peer carries a strictly greater cluster-config version than
// ours. The replication layer uses this to schedule a pull.
type VersionObserver func(peerID, peerAddr string, peerVersion uint64)

// Manager coordinates heartbeats and master election for one node.
type Manager struct {
	selfID        string
	selfAdvertise string
	cluster       *config.ClusterConfig
	client        *transport.Client

	heartbeatInterval time.Duration
	deadAfter         time.Duration

	mu       sync.RWMutex
	term     uint64
	masterID string
	lastSeen map[string]time.Time // peerID -> last contact (sent or recv)
	addrOf   map[string]string    // peerID -> advertise addr (last known)

	observer VersionObserver
}

// New constructs a Manager bound to the given identity, cluster config,
// and RPC client. The Manager does not start any goroutines until
// Start is called.
func New(selfID string, cluster *config.ClusterConfig, client *transport.Client) *Manager {
	return &Manager{
		selfID:            selfID,
		cluster:           cluster,
		client:            client,
		heartbeatInterval: DefaultHeartbeatInterval,
		deadAfter:         DefaultDeadAfter,
		lastSeen:          map[string]time.Time{},
		addrOf:            map[string]string{},
	}
}

// SetSelfAdvertise records the address this node advertises to peers.
// It's piggy-backed on every outbound heartbeat so the recipient can
// reach us even before we appear in their cluster.yaml.
func (m *Manager) SetSelfAdvertise(addr string) {
	m.mu.Lock()
	m.selfAdvertise = addr
	m.mu.Unlock()
}

// SetVersionObserver registers a callback fired when a peer reports a
// higher cluster-config version than ours.
func (m *Manager) SetVersionObserver(fn VersionObserver) {
	m.observer = fn
}

// Start spins up the heartbeat loop and the election ticker.
// Returns when ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	// Mark self live so a one-node cluster elects itself on tick zero.
	m.markLive(m.selfID)
	m.recomputeMaster()

	t := time.NewTicker(m.heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

// HandleHeartbeat is the inbound RPC handler. Records the sender as
// live and returns our current view of term, master, and version.
func (m *Manager) HandleHeartbeat(req transport.HeartbeatRequest) transport.HeartbeatResponse {
	if req.FromNodeID != "" && req.FromNodeID != m.selfID {
		m.markLive(req.FromNodeID)
		if req.Advertise != "" {
			m.mu.Lock()
			m.addrOf[req.FromNodeID] = req.Advertise
			m.mu.Unlock()
		}
		m.maybeNotifyVersion(req.FromNodeID, req.Version)
	}
	m.recomputeMaster()
	snap := m.cluster.Snapshot()
	m.mu.RLock()
	selfAdv := m.selfAdvertise
	m.mu.RUnlock()
	return transport.HeartbeatResponse{
		NodeID:    m.selfID,
		Advertise: selfAdv,
		Term:      m.Term(),
		MasterID:  m.Master(),
		Version:   snap.Version,
	}
}

// Master returns the currently-elected master NodeID. Empty when the
// cluster has no quorum.
func (m *Manager) Master() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.masterID
}

// IsMaster is a convenience predicate.
func (m *Manager) IsMaster() bool {
	return m.Master() == m.selfID
}

// Term returns the current election term.
func (m *Manager) Term() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.term
}

// HasQuorum reports whether the live set is large enough to elect a
// master.
func (m *Manager) HasQuorum() bool {
	live := m.LiveSet()
	return len(live) >= m.cluster.QuorumSize()
}

// LiveSet returns a copy of the currently-live NodeIDs.
func (m *Manager) LiveSet() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cutoff := time.Now().Add(-m.deadAfter)
	out := make([]string, 0, len(m.lastSeen)+1)
	for id, ts := range m.lastSeen {
		if ts.After(cutoff) || id == m.selfID {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Liveness returns the peer ID → last_seen map snapshot for status.
func (m *Manager) Liveness() map[string]time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]time.Time, len(m.lastSeen))
	for k, v := range m.lastSeen {
		out[k] = v
	}
	return out
}

// tick fires one round of heartbeats to all peers (except self) and
// then re-runs the election.
func (m *Manager) tick(ctx context.Context) {
	snap := m.cluster.Snapshot()
	// remember addresses so we can dial peers even if cluster.yaml shifts
	for _, p := range snap.Peers {
		if p.NodeID != "" && p.Advertise != "" {
			m.mu.Lock()
			m.addrOf[p.NodeID] = p.Advertise
			m.mu.Unlock()
		}
	}

	currentMaster := m.Master()
	m.mu.RLock()
	selfAdv := m.selfAdvertise
	m.mu.RUnlock()
	for _, p := range snap.Peers {
		if p.NodeID == m.selfID || p.NodeID == "" || p.Advertise == "" {
			continue
		}
		peerID, addr := p.NodeID, p.Advertise

		go func(peerID, addr string) {
			callCtx, cancel := context.WithTimeout(ctx, m.heartbeatInterval)
			defer cancel()
			req := transport.HeartbeatRequest{
				FromNodeID: m.selfID,
				Advertise:  selfAdv,
				Term:       m.Term(),
				MasterID:   currentMaster,
				Version:    snap.Version,
			}
			var resp transport.HeartbeatResponse
			if err := m.client.Call(callCtx, peerID, addr,
				transport.MethodHeartbeat, req, &resp); err != nil {
				return
			}
			m.markLive(peerID)
			if resp.Advertise != "" {
				m.mu.Lock()
				m.addrOf[peerID] = resp.Advertise
				m.mu.Unlock()
			}
			m.maybeNotifyVersion(peerID, resp.Version)
		}(peerID, addr)
	}

	m.markLive(m.selfID)
	m.recomputeMaster()
}

func (m *Manager) markLive(id string) {
	m.mu.Lock()
	m.lastSeen[id] = time.Now()
	m.mu.Unlock()
}

func (m *Manager) maybeNotifyVersion(peerID string, peerVer uint64) {
	if m.observer == nil {
		return
	}
	local := m.cluster.Snapshot().Version
	if peerVer <= local {
		return
	}
	m.mu.RLock()
	addr := m.addrOf[peerID]
	m.mu.RUnlock()
	// Without an address we can't pull — silently wait for a
	// later heartbeat (or for the peer to land in cluster.yaml) to
	// supply one. Firing the observer with addr == "" would just
	// produce log spam from the replicator.
	if addr == "" {
		return
	}
	m.observer(peerID, addr, peerVer)
}

func (m *Manager) recomputeMaster() {
	live := m.LiveSet()
	quorum := m.cluster.QuorumSize()

	m.mu.Lock()
	defer m.mu.Unlock()

	var newMaster string
	if len(live) >= quorum && len(live) > 0 {
		newMaster = live[0] // lowest NodeID wins
	}
	if newMaster != m.masterID {
		m.term++
		m.masterID = newMaster
	}
}
