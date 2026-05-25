package checks

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// maxBodyRead is the cap on how much body a check will pull when
// BodyMatch is non-empty. Anything beyond is ignored.
const maxBodyRead = 1 << 20 // 1 MiB

type httpProber struct{}

func (httpProber) Probe(ctx context.Context, c *config.Check) Result {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(dctx context.Context, network, addr string) (net.Conn, error) {
			return dialWithResolver(dctx, c, network, addr)
		},
	}
	client := &http.Client{
		Timeout:   c.Timeout,
		Transport: transport,
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Target, nil)
	if err != nil {
		return Result{OK: false, Detail: "build request: " + err.Error()}
	}
	req.Header.Set("User-Agent", "quptime/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return Result{OK: false, Detail: err.Error(), Latency: time.Since(start)}
	}
	defer resp.Body.Close()

	expected := c.ExpectStatus
	if expected == 0 {
		expected = 200
	}
	if resp.StatusCode != expected {
		return Result{
			OK:      false,
			Detail:  "status " + resp.Status,
			Latency: time.Since(start),
		}
	}

	if c.BodyMatch != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
		if err != nil {
			return Result{OK: false, Detail: "read body: " + err.Error(), Latency: time.Since(start)}
		}
		if !strings.Contains(string(body), c.BodyMatch) {
			return Result{OK: false, Detail: "body match miss", Latency: time.Since(start)}
		}
	}

	return Result{OK: true, Latency: time.Since(start)}
}
