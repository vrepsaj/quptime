package checks

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

type dnsProber struct{}

// Probe runs a DNS lookup against the configured resolver (or the
// system resolver if none) and validates the answer set. Empty
// answers always fail; if DNSExpect is set, at least one answer must
// contain that substring (case-insensitive).
func (dnsProber) Probe(ctx context.Context, c *config.Check) Result {
	host := strings.TrimSpace(c.Target)
	if host == "" {
		return Result{OK: false, Detail: "empty target"}
	}
	record := strings.ToLower(strings.TrimSpace(c.DNSRecord))
	if record == "" {
		record = "a"
	}

	resolver := net.DefaultResolver
	if r := strings.TrimSpace(c.DNSResolver); r != "" {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(dctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: c.Timeout}
				return d.DialContext(dctx, network, r)
			},
		}
	}

	start := time.Now()
	answers, err := lookupRecord(ctx, resolver, record, host)
	latency := time.Since(start)
	if err != nil {
		return Result{OK: false, Detail: err.Error(), Latency: latency}
	}
	if len(answers) == 0 {
		return Result{OK: false, Detail: fmt.Sprintf("no %s records for %s", strings.ToUpper(record), host), Latency: latency}
	}
	if want := strings.TrimSpace(c.DNSExpect); want != "" {
		needle := strings.ToLower(want)
		matched := false
		for _, a := range answers {
			if strings.Contains(strings.ToLower(a), needle) {
				matched = true
				break
			}
		}
		if !matched {
			return Result{
				OK:      false,
				Detail:  fmt.Sprintf("no answer contains %q (got %s)", want, summariseAnswers(answers)),
				Latency: latency,
			}
		}
	}
	return Result{OK: true, Latency: latency}
}

// lookupRecord dispatches the resolver call by record type. Returns
// the answers as strings for uniform substring matching downstream.
func lookupRecord(ctx context.Context, r *net.Resolver, record, host string) ([]string, error) {
	switch record {
	case "a":
		ips, err := r.LookupIP(ctx, "ip4", host)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(ips))
		for i, ip := range ips {
			out[i] = ip.String()
		}
		return out, nil
	case "aaaa":
		ips, err := r.LookupIP(ctx, "ip6", host)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(ips))
		for i, ip := range ips {
			out[i] = ip.String()
		}
		return out, nil
	case "cname":
		cname, err := r.LookupCNAME(ctx, host)
		if err != nil {
			return nil, err
		}
		if cname == "" {
			return nil, nil
		}
		return []string{cname}, nil
	case "mx":
		mxs, err := r.LookupMX(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(mxs))
		for i, mx := range mxs {
			out[i] = fmt.Sprintf("%d %s", mx.Pref, mx.Host)
		}
		return out, nil
	case "txt":
		return r.LookupTXT(ctx, host)
	case "ns":
		nss, err := r.LookupNS(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(nss))
		for i, ns := range nss {
			out[i] = ns.Host
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported record type %q (want a|aaaa|cname|mx|txt|ns)", record)
	}
}

// summariseAnswers joins answers for inclusion in a failure Detail,
// trimming long lists so we don't ship a kilobyte of TXT data through
// the alert pipeline.
func summariseAnswers(answers []string) string {
	const max = 3
	if len(answers) <= max {
		return strings.Join(answers, ", ")
	}
	return strings.Join(answers[:max], ", ") + fmt.Sprintf(", … +%d more", len(answers)-max)
}
