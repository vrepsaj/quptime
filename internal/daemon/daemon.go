// Package daemon ties every long-running component together.
//
// Lifecycle
//
//   - Load node identity, cluster config, trust store, and key material.
//   - Build a transport.Client + transport.Server, share TLS assets.
//   - Construct the quorum manager, replicator, aggregator and alert
//     dispatcher; wire transport handlers; wire the version observer
//     to the replicator's pull path; gate alert dispatch on
//     "I am the master".
//   - Start the inter-node listener, the local unix-socket control
//     plane, the heartbeat loop and the check scheduler.
//   - On ctx cancel, gracefully tear everything down.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"git.cer.sh/axodouble/quptime/internal/alerts"
	"git.cer.sh/axodouble/quptime/internal/checks"
	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/crypto"
	"git.cer.sh/axodouble/quptime/internal/quorum"
	"git.cer.sh/axodouble/quptime/internal/replicate"
	"git.cer.sh/axodouble/quptime/internal/transport"
	"git.cer.sh/axodouble/quptime/internal/trust"
)

// Daemon is the live process: every long-running component lives here.
type Daemon struct {
	logger *log.Logger

	node    *config.NodeConfig
	cluster *config.ClusterConfig
	trust   *trust.Store

	assets *transport.TLSAssets
	client *transport.Client
	server *transport.Server

	quorum     *quorum.Manager
	replicator *replicate.Replicator
	aggregator *checks.Aggregator
	dispatcher *alerts.Dispatcher
	scheduler  *checks.Scheduler

	control *controlServer
	wg      sync.WaitGroup
}

// New loads every persistent piece of state and assembles the daemon.
// It does not start any goroutines.
func New(logger *log.Logger) (*Daemon, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "quptime: ", log.LstdFlags|log.Lmsgprefix)
	}

	node, err := config.LoadNodeConfig()
	if err != nil {
		return nil, fmt.Errorf("load node.yaml: %w", err)
	}
	if node.NodeID == "" {
		return nil, errors.New("node.yaml has empty node_id — run `qu init` first")
	}
	// Upgrade path: a node.yaml from before the enrollment-token rework
	// will still carry the now-unused cluster_secret field. Blank it
	// out and rewrite so it stops sitting on disk as a tempting target.
	// Trust between existing peers is unaffected (it lives in trust.yaml
	// + the cert material replicated through cluster.yaml).
	if node.ClusterSecret != "" {
		logger.Printf("node.yaml: clearing legacy cluster_secret field (enrollment tokens replace it)")
		node.ClusterSecret = ""
		if err := node.Save(); err != nil {
			return nil, fmt.Errorf("rewrite node.yaml without cluster_secret: %w", err)
		}
	}

	cluster, err := config.LoadClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load cluster.yaml: %w", err)
	}

	store, err := trust.Load()
	if err != nil {
		return nil, fmt.Errorf("load trust.yaml: %w", err)
	}

	priv, err := crypto.LoadPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}
	certPEM, err := crypto.LoadCertPEM()
	if err != nil {
		return nil, fmt.Errorf("load cert: %w", err)
	}

	assets := &transport.TLSAssets{Cert: certPEM, Key: priv, Trust: store}
	client := transport.NewClient(assets)
	server := transport.NewServer(assets)

	d := &Daemon{
		logger:  logger,
		node:    node,
		cluster: cluster,
		trust:   store,
		assets:  assets,
		client:  client,
		server:  server,
	}

	d.quorum = quorum.New(node.NodeID, cluster, client)
	d.quorum.SetSelfAdvertise(node.AdvertiseAddr())
	d.replicator = replicate.New(node.NodeID, cluster, client, d.quorum)
	d.aggregator = checks.NewAggregator(cluster, nil)
	d.dispatcher = alerts.New(cluster, node.NodeID, logger)

	d.aggregator.SetTransition(func(check *config.Check, from, to checks.State, snap checks.Snapshot) {
		if !d.quorum.IsMaster() {
			return
		}
		d.dispatcher.OnTransition(check, from, to, snap)
	})

	d.quorum.SetVersionObserver(func(peerID, peerAddr string, peerVer uint64) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.replicator.PullFrom(ctx, peerID, peerAddr); err != nil {
			d.logger.Printf("replicate: pull from %s: %v", peerID, err)
		}
	})

	d.scheduler = checks.NewScheduler(cluster, &sink{d: d})
	d.control = newControlServer(d)
	d.registerHandlers()

	// Whenever cluster.yaml changes, mirror peer certs into the local
	// trust store so this node can mTLS to every other peer — even
	// peers it was never invited by directly.
	cluster.OnChange(d.syncTrustFromCluster)
	d.syncTrustFromCluster()

	return d, nil
}

// syncTrustFromCluster makes sure every peer listed in cluster.yaml
// has a corresponding trust entry. Trust entries are only added (not
// removed) here — `qu node remove` is the explicit eviction path.
func (d *Daemon) syncTrustFromCluster() {
	snap := d.cluster.Snapshot()
	for _, p := range snap.Peers {
		if p.NodeID == "" || p.NodeID == d.node.NodeID {
			continue
		}
		if p.Fingerprint == "" || p.CertPEM == "" {
			continue // pre-1.0 peer entry without cert material — skip
		}
		if existing, ok := d.trust.Get(p.NodeID); ok && existing.Fingerprint == p.Fingerprint {
			continue
		}
		if err := d.trust.Add(trust.Entry{
			NodeID:      p.NodeID,
			Address:     p.Advertise,
			Fingerprint: p.Fingerprint,
			CertPEM:     p.CertPEM,
		}); err != nil {
			d.logger.Printf("trust sync: %s: %v", p.NodeID, err)
		}
	}
}

// Run binds the inter-node listener and the local control socket,
// starts the quorum loop and the scheduler, and blocks until ctx is
// cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", d.node.BindAddr, d.node.BindPort)
	d.logger.Printf("listening on %s as node %s", addr, d.node.NodeID)

	servErr := make(chan error, 1)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		servErr <- d.server.Serve(ctx, addr)
	}()

	ctrlErr := make(chan error, 1)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctrlErr <- d.control.Serve(ctx)
	}()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.quorum.Start(ctx)
	}()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.scheduler.Start(ctx)
	}()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.watchManualEdits(ctx)
	}()

	select {
	case <-ctx.Done():
	case err := <-servErr:
		if err != nil {
			d.logger.Printf("transport server exited: %v", err)
		}
	case err := <-ctrlErr:
		if err != nil {
			d.logger.Printf("control server exited: %v", err)
		}
	}

	d.server.Stop()
	d.control.Stop()
	d.client.Close()
	d.wg.Wait()
	return nil
}

// sink routes scheduled probe results either into the local
// aggregator (when self is master) or to the current master over
// RPC. Implements checks.Sink.
type sink struct{ d *Daemon }

func (s *sink) Submit(r checks.Result) {
	if s.d.quorum.IsMaster() {
		s.d.aggregator.Submit(s.d.node.NodeID, r)
		return
	}
	masterID := s.d.quorum.Master()
	if masterID == "" {
		return // no master right now — drop; we'll probe again next interval
	}
	addr := s.d.addressOf(masterID)
	if addr == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := transport.ReportResultRequest{
		FromNodeID: s.d.node.NodeID,
		CheckID:    r.CheckID,
		OK:         r.OK,
		Detail:     r.Detail,
		LatencyMS:  r.Latency.Milliseconds(),
		At:         r.Timestamp,
	}
	if err := s.d.client.Call(ctx, masterID, addr, transport.MethodReportResult, req, nil); err != nil {
		s.d.logger.Printf("report to master %s: %v", masterID, err)
	}
}

func (d *Daemon) addressOf(nodeID string) string {
	for _, p := range d.cluster.Snapshot().Peers {
		if p.NodeID == nodeID {
			return p.Advertise
		}
	}
	return ""
}
