// Package checks runs the configured HTTP/TCP/ICMP probes on every
// node and aggregates per-check results on the master.
//
// Probers are the small "do one round of work" units. The Scheduler
// drives them on a per-check timer and ships each result back to the
// master via the inter-node transport. The Aggregator (master only)
// folds incoming results into per-check sliding windows and decides
// when a check has crossed UP↔DOWN.
package checks

import (
	"context"
	"fmt"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// Result is the outcome of a single probe.
type Result struct {
	CheckID   string
	OK        bool
	Detail    string
	Latency   time.Duration
	Timestamp time.Time
}

// Prober runs one probe of a configured check.
type Prober interface {
	Probe(ctx context.Context, c *config.Check) Result
}

// Run dispatches to the right Prober for the given check type. Returns
// an error result instead of failing when a check has unknown type.
func Run(ctx context.Context, c *config.Check) Result {
	deadline := c.Timeout
	if deadline <= 0 {
		deadline = 10 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var p Prober
	switch c.Type {
	case config.CheckHTTP:
		p = httpProber{}
	case config.CheckTCP:
		p = tcpProber{}
	case config.CheckICMP:
		p = icmpProber{}
	case config.CheckTLS:
		p = tlsProber{}
	case config.CheckDNS:
		p = dnsProber{}
	default:
		return Result{
			CheckID:   c.ID,
			OK:        false,
			Detail:    fmt.Sprintf("unknown check type %q", c.Type),
			Timestamp: time.Now().UTC(),
		}
	}
	res := p.Probe(pctx, c)
	res.CheckID = c.ID
	if res.Timestamp.IsZero() {
		res.Timestamp = time.Now().UTC()
	}
	return res
}
