package checks

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

func TestNormaliseResolvers(t *testing.T) {
	t.Parallel()
	got := normaliseResolvers([]string{"1.1.1.1", "  ", "1.0.0.1:5353", " 8.8.8.8 ", "[2606:4700:4700::1111]:53"})
	want := []string{"1.1.1.1:53", "1.0.0.1:5353", "8.8.8.8:53", "[2606:4700:4700::1111]:53"}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEffectiveResolvers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		check  config.Check
		def    []string
		expect []string
	}{
		{
			name:   "check overrides cluster",
			check:  config.Check{Type: config.CheckHTTP, Resolvers: []string{"1.1.1.1"}},
			def:    []string{"8.8.8.8"},
			expect: []string{"1.1.1.1:53"},
		},
		{
			name:   "falls back to cluster default",
			check:  config.Check{Type: config.CheckHTTP},
			def:    []string{"8.8.8.8", "8.8.4.4"},
			expect: []string{"8.8.8.8:53", "8.8.4.4:53"},
		},
		{
			name:   "legacy DNSResolver only for DNS checks",
			check:  config.Check{Type: config.CheckDNS, DNSResolver: "9.9.9.9"},
			def:    nil,
			expect: []string{"9.9.9.9:53"},
		},
		{
			name:   "legacy DNSResolver ignored on non-DNS",
			check:  config.Check{Type: config.CheckHTTP, DNSResolver: "9.9.9.9"},
			def:    nil,
			expect: nil,
		},
		{
			name:   "nothing set means system resolver",
			check:  config.Check{Type: config.CheckHTTP},
			def:    nil,
			expect: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveResolvers(&tc.check, tc.def)
			if len(got) != len(tc.expect) {
				t.Fatalf("len=%d, want %d (got=%v want=%v)", len(got), len(tc.expect), got, tc.expect)
			}
			for i := range tc.expect {
				if got[i] != tc.expect[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tc.expect[i])
				}
			}
		})
	}
}

// TestResolverFailover spins up two loopback UDP listeners; the first
// refuses to answer (dropped packet) and the second responds. Verifies
// that the Dial-level failover threads through to the second.
//
// We can't fully test the resolver's wire protocol without an actual
// DNS responder, so we verify the cheaper invariant: when the first
// listener is closed (refused), Dial transparently falls through to
// the second and the connection succeeds.
func TestResolverFailover_DialLevel(t *testing.T) {
	t.Parallel()

	// Bind an ephemeral port, then close — so dialing it gets "connection
	// refused". (UDP "connect" succeeds locally either way, so use TCP
	// since that's what net.Resolver requests through Dial when needed
	// for large/truncated responses. Either way, the important thing
	// is exercising the failover loop.)
	bad, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	badAddr := bad.Addr().String()
	bad.Close() // released; subsequent dials will be refused

	// Working listener — we just accept and immediately close, so the
	// dial succeeds even though it'll be useless for DNS. That's enough
	// to prove the failover loop.
	good, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	goodAddr := good.Addr().String()
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := good.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			conn.Close()
		}
	}()

	r := resolverFor([]string{badAddr, goodAddr}, 1*time.Second)
	if r == nil {
		t.Fatal("resolverFor returned nil for a non-empty list")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := r.Dial(ctx, "tcp", "doesnt-matter")
	if err != nil {
		t.Fatalf("Dial fell through both: %v", err)
	}
	conn.Close()
	if accepted.Load() == 0 {
		t.Errorf("expected the working listener to be the one that accepted")
	}
}

func TestResolverFor_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	if r := resolverFor(nil, time.Second); r != nil {
		t.Errorf("empty list should return nil, got %+v", r)
	}
	if r := resolverFor([]string{"", "  "}, time.Second); r != nil {
		t.Errorf("all-blank list should return nil, got %+v", r)
	}
}

func TestResolveOne_LiteralIPPassesThrough(t *testing.T) {
	t.Parallel()
	c := &config.Check{Type: config.CheckICMP, Resolvers: []string{"1.1.1.1"}, Timeout: time.Second}
	got, err := resolveOne(context.Background(), c, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.0.0.1" {
		t.Errorf("literal IP should pass through unchanged, got %q", got)
	}
}

func TestResolveOne_NoOverrideReturnsHost(t *testing.T) {
	t.Parallel()
	c := &config.Check{Type: config.CheckICMP, Timeout: time.Second}
	got, err := resolveOne(context.Background(), c, "example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	// With no resolver override the helper hands the original hostname
	// straight to the caller so pro-bing can do its own lookup.
	if got != "example.invalid" {
		t.Errorf("no-override path should return host unchanged, got %q", got)
	}
}
