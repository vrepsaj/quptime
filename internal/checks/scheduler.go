package checks

import (
	"context"
	"sync"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// ReconcileInterval is how often the scheduler reconciles its set of
// running probes against cluster.yaml.
const ReconcileInterval = 5 * time.Second

// Sink is the abstraction the scheduler uses to report results.
// Implemented by the daemon: results go straight to the local
// aggregator when self is the master, otherwise they ship to the
// master over the RPC channel.
type Sink interface {
	Submit(Result)
}

// Scheduler keeps a goroutine alive per configured check. On each
// reconcile pass it starts probes for new checks, stops probes for
// removed checks, and restarts probes whose interval or type changed.
type Scheduler struct {
	cluster *config.ClusterConfig
	sink    Sink

	mu      sync.Mutex
	running map[string]*probeWorker
}

type probeWorker struct {
	check  config.Check
	cancel context.CancelFunc
}

// NewScheduler creates a scheduler bound to the given cluster config.
func NewScheduler(cluster *config.ClusterConfig, sink Sink) *Scheduler {
	return &Scheduler{
		cluster: cluster,
		sink:    sink,
		running: map[string]*probeWorker{},
	}
}

// Start runs the reconcile loop until ctx is cancelled. Reconcile is
// also called immediately on entry so checks start without waiting
// for the first tick.
func (s *Scheduler) Start(ctx context.Context) {
	s.reconcile(ctx)
	t := time.NewTicker(ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopAll()
			return
		case <-t.C:
			s.reconcile(ctx)
		}
	}
}

func (s *Scheduler) reconcile(ctx context.Context) {
	snap := s.cluster.Snapshot()
	want := map[string]config.Check{}
	for _, c := range snap.Checks {
		if c.ID == "" || c.Disabled {
			continue
		}
		want[c.ID] = c
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for id, w := range s.running {
		desired, stillThere := want[id]
		if !stillThere || !sameCheck(desired, w.check) {
			w.cancel()
			delete(s.running, id)
		}
	}
	for id, c := range want {
		if _, exists := s.running[id]; exists {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		s.running[id] = &probeWorker{check: c, cancel: cancel}
		go s.run(wctx, c)
	}
}

func (s *Scheduler) run(ctx context.Context, c config.Check) {
	interval := c.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	// stagger startup so a freshly-loaded scheduler doesn't burst
	// hundreds of probes simultaneously.
	jitter := time.Duration(int64(interval) / 10)
	if jitter > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(time.Now().UnixNano() % int64(jitter))):
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// fire one immediate probe so state populates without delay.
	s.sink.Submit(Run(ctx, &c))

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sink.Submit(Run(ctx, &c))
		}
	}
}

func (s *Scheduler) stopAll() {
	s.mu.Lock()
	for id, w := range s.running {
		w.cancel()
		delete(s.running, id)
	}
	s.mu.Unlock()
}

// sameCheck returns true when two Check structs would produce
// identical probing behaviour, so the scheduler can leave the worker
// running across a no-op config push.
func sameCheck(a, b config.Check) bool {
	return a.ID == b.ID &&
		a.Type == b.Type &&
		a.Target == b.Target &&
		a.Interval == b.Interval &&
		a.Timeout == b.Timeout &&
		a.ExpectStatus == b.ExpectStatus &&
		a.BodyMatch == b.BodyMatch
}
