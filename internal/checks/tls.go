package checks

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// defaultTLSWarnDays is how close to expiry a cert can be before the
// TLS check flips DOWN. Operators pick a longer window via
// Check.TLSWarnDays when they want more lead time.
const defaultTLSWarnDays = 14

type tlsProber struct{}

// Probe dials the target over TLS without verifying the peer cert
// chain (we only care about expiry, and self-signed targets are a
// legitimate use case), then folds the leaf's NotAfter into the
// result. Target may be a bare "host:port" or a full URL — anything
// without an explicit port is treated as host:443.
func (tlsProber) Probe(ctx context.Context, c *config.Check) Result {
	addr, sni, err := tlsDialAddr(c.Target, c.TLSServerName)
	if err != nil {
		return Result{OK: false, Detail: err.Error()}
	}

	start := time.Now()
	rawConn, err := dialWithResolver(ctx, c, "tcp", addr)
	if err != nil {
		return Result{OK: false, Detail: err.Error(), Latency: time.Since(start)}
	}
	tc := tls.Client(rawConn, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         sni,
		InsecureSkipVerify: true, // expiry-focused; chain validity is out of scope here
	})
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return Result{OK: false, Detail: err.Error(), Latency: time.Since(start)}
	}
	defer tc.Close()
	latency := time.Since(start)

	state := tc.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return Result{OK: false, Detail: "peer presented no certificate", Latency: latency}
	}
	leaf := state.PeerCertificates[0]

	warnDays := c.TLSWarnDays
	if warnDays <= 0 {
		warnDays = defaultTLSWarnDays
	}
	remaining := time.Until(leaf.NotAfter)
	if remaining <= 0 {
		return Result{
			OK:      false,
			Detail:  fmt.Sprintf("cert expired %s ago (notAfter=%s)", roundDur(-remaining), leaf.NotAfter.UTC().Format(time.RFC3339)),
			Latency: latency,
		}
	}
	if remaining < time.Duration(warnDays)*24*time.Hour {
		return Result{
			OK:      false,
			Detail:  fmt.Sprintf("cert expires in %s (notAfter=%s)", roundDur(remaining), leaf.NotAfter.UTC().Format(time.RFC3339)),
			Latency: latency,
		}
	}
	return Result{OK: true, Latency: latency}
}

// tlsDialAddr normalises Target into a "host:port" pair plus the SNI
// to present. A bare hostname defaults to :443. URLs are accepted so
// operators can paste an https:// link directly.
func tlsDialAddr(target, overrideSNI string) (addr, sni string, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", fmt.Errorf("empty target")
	}

	host, port := "", ""
	switch {
	case strings.Contains(target, "://"):
		u, perr := url.Parse(target)
		if perr != nil {
			return "", "", fmt.Errorf("parse target: %w", perr)
		}
		host = u.Hostname()
		port = u.Port()
	default:
		h, p, splitErr := net.SplitHostPort(target)
		if splitErr == nil {
			host, port = h, p
		} else {
			host = target
		}
	}
	if host == "" {
		return "", "", fmt.Errorf("no host in target %q", target)
	}
	if port == "" {
		port = "443"
	}
	sni = overrideSNI
	if sni == "" {
		sni = host
	}
	return net.JoinHostPort(host, port), sni, nil
}

// roundDur formats a duration for human display. Days for >24h, then
// hours, then minutes — we never need sub-minute precision for a cert
// expiry message.
func roundDur(d time.Duration) string {
	if d >= 24*time.Hour {
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd", days)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.Round(time.Second).String()
}
