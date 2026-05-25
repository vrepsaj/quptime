package checks

import (
	"context"
	"runtime"
	"time"

	probing "github.com/prometheus-community/pro-bing"

	"git.cer.sh/axodouble/quptime/internal/config"
)

type icmpProber struct{}

// Probe sends a single ICMP echo. On Linux we default to unprivileged
// "udp" mode so the daemon does not require CAP_NET_RAW. Operators
// who can grant the cap (or run as root) get raw ICMP automatically.
func (icmpProber) Probe(ctx context.Context, c *config.Check) Result {
	start := time.Now()
	target, err := resolveOne(ctx, c, c.Target)
	if err != nil {
		return Result{OK: false, Detail: "resolve: " + err.Error()}
	}
	pinger, err := probing.NewPinger(target)
	if err != nil {
		return Result{OK: false, Detail: "resolve: " + err.Error()}
	}
	pinger.Count = 1
	pinger.Timeout = c.Timeout
	if pinger.Timeout <= 0 {
		pinger.Timeout = 5 * time.Second
	}
	pinger.SetPrivileged(runtime.GOOS != "linux")

	doneCh := make(chan struct{})
	go func() {
		_ = pinger.RunWithContext(ctx)
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-ctx.Done():
		pinger.Stop()
		return Result{OK: false, Detail: "timeout", Latency: time.Since(start)}
	}

	stats := pinger.Statistics()
	if stats.PacketsRecv == 0 {
		return Result{OK: false, Detail: "no reply", Latency: time.Since(start)}
	}
	return Result{OK: true, Latency: stats.AvgRtt}
}
