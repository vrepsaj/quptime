package checks

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// TestDNSProberLocalhost relies on the system resolver returning the
// "localhost" name. Every reasonable test environment maps this either
// via /etc/hosts or the resolver's built-in fallback, so this is a
// safe network-free smoke test.
func TestDNSProberLocalhost(t *testing.T) {
	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckDNS, Target: "localhost",
		Timeout: 5 * time.Second, DNSRecord: "a",
	})
	if !res.OK {
		t.Errorf("expected localhost A to resolve, got %+v", res)
	}
}

func TestDNSProberExpectMatch(t *testing.T) {
	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckDNS, Target: "localhost",
		Timeout: 5 * time.Second, DNSRecord: "a",
		DNSExpect: "127.",
	})
	if !res.OK {
		t.Errorf("expected localhost A to contain 127., got %+v", res)
	}
}

func TestDNSProberExpectMiss(t *testing.T) {
	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckDNS, Target: "localhost",
		Timeout: 5 * time.Second, DNSRecord: "a",
		DNSExpect: "203.0.113.99",
	})
	if res.OK {
		t.Errorf("expected miss, got %+v", res)
	}
	if !strings.Contains(res.Detail, "no answer contains") {
		t.Errorf("detail should explain miss, got %q", res.Detail)
	}
}

func TestDNSProberUnsupportedRecord(t *testing.T) {
	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckDNS, Target: "localhost",
		Timeout: 5 * time.Second, DNSRecord: "spf",
	})
	if res.OK {
		t.Errorf("unsupported record type should fail, got %+v", res)
	}
	if !strings.Contains(res.Detail, "unsupported record type") {
		t.Errorf("detail should mention unsupported, got %q", res.Detail)
	}
}

func TestDNSProberEmptyTarget(t *testing.T) {
	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckDNS, Target: "",
		Timeout: 5 * time.Second,
	})
	if res.OK {
		t.Errorf("empty target should fail, got %+v", res)
	}
}
