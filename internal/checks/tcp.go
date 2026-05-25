package checks

import (
	"context"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

type tcpProber struct{}

func (tcpProber) Probe(ctx context.Context, c *config.Check) Result {
	start := time.Now()
	conn, err := dialWithResolver(ctx, c, "tcp", c.Target)
	if err != nil {
		return Result{OK: false, Detail: err.Error(), Latency: time.Since(start)}
	}
	_ = conn.Close()
	return Result{OK: true, Latency: time.Since(start)}
}
