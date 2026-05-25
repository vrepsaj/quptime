package checks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// defaultResolverPort is appended when an operator gives a bare IP /
// hostname without an explicit port. Port 53 is the IANA registered
// well-known port for DNS.
const defaultResolverPort = "53"

// EffectiveResolvers picks the DNS-server list that should be used to
// resolve the check's target. Precedence (highest first):
//
//  1. check.Resolvers — the per-check override.
//  2. clusterDefaults — ClusterConfig.Resolvers.
//  3. For DNS-type checks only, the legacy check.DNSResolver field as a
//     single-entry list (kept for backward compatibility with configs
//     written before the multi-resolver field existed).
//
// An empty result means "no override" — callers should fall back to
// the host's system resolver.
func EffectiveResolvers(check *config.Check, clusterDefaults []string) []string {
	if check == nil {
		return nil
	}
	if list := normaliseResolvers(check.Resolvers); len(list) > 0 {
		return list
	}
	if list := normaliseResolvers(clusterDefaults); len(list) > 0 {
		return list
	}
	if check.Type == config.CheckDNS {
		if s := strings.TrimSpace(check.DNSResolver); s != "" {
			return normaliseResolvers([]string{s})
		}
	}
	return nil
}

// normaliseResolvers trims, drops blanks, and appends ":53" to any
// entry that doesn't already carry an explicit port.
func normaliseResolvers(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(s, defaultResolverPort)
		}
		out = append(out, s)
	}
	return out
}

// resolverFor returns a *net.Resolver wired to walk the given server
// list with connection-level failover. The Dial closure tries each
// server in order, returning the first that connects. timeout caps
// each individual dial, not the overall lookup.
//
// Returns nil when servers is empty — caller should use the system
// resolver (net.DefaultResolver) in that case.
func resolverFor(servers []string, timeout time.Duration) *net.Resolver {
	servers = normaliseResolvers(servers)
	if len(servers) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			var lastErr error
			for _, s := range servers {
				conn, err := d.DialContext(ctx, network, s)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("no resolvers configured")
			}
			return nil, lastErr
		},
	}
}

// resolverForCheck is a small convenience for probers that have a
// *config.Check in hand. Returns the system resolver when the check
// carries no override.
func resolverForCheck(c *config.Check) *net.Resolver {
	if r := resolverFor(c.Resolvers, c.Timeout); r != nil {
		return r
	}
	return net.DefaultResolver
}

// dialWithResolver opens a TCP connection to addr ("host:port") using
// the check's effective resolver. The hostname in addr is looked up
// against the configured DNS servers (if any), and each returned IP
// is tried in turn until one connects. If addr already holds an IP,
// the dial happens directly.
//
// The returned conn behaves like net.Dialer.DialContext would have
// returned; callers can treat it as a regular TCP socket.
func dialWithResolver(ctx context.Context, c *config.Check, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("split host:port %q: %w", addr, err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := net.Dialer{Timeout: timeout}

	// Literal IPs skip the resolver entirely — pointless lookup, and
	// also lets operators target bare addresses without configuring
	// resolvers.
	if ip := net.ParseIP(host); ip != nil {
		return d.DialContext(ctx, network, addr)
	}

	resolver := resolverForCheck(c)
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// resolveOne is the ICMP path: pro-bing wants a single hostname-or-IP
// string to ping. When the check has resolver overrides, we look the
// host up ourselves and hand pro-bing the first IP. The original
// hostname is preserved in the failure detail when something goes
// wrong with the lookup itself.
func resolveOne(ctx context.Context, c *config.Check, host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("empty target")
	}
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	if len(normaliseResolvers(c.Resolvers)) == 0 {
		// No override — let pro-bing do its own lookup so we don't
		// gratuitously change ICMP behaviour for the common case.
		return host, nil
	}
	resolver := resolverForCheck(c)
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("resolve %s: no addresses", host)
	}
	return ips[0].String(), nil
}
