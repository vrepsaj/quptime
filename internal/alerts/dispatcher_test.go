package alerts

import (
	"strings"
	"testing"

	"git.cer.sh/axodouble/quptime/internal/checks"
	"git.cer.sh/axodouble/quptime/internal/config"
)

func TestShouldAlertFiltersColdStartUp(t *testing.T) {
	cases := []struct {
		name string
		from checks.State
		to   checks.State
		want bool
	}{
		{"cold start to up (master failover / daemon restart)", checks.StateUnknown, checks.StateUp, false},
		{"cold start to down still alerts", checks.StateUnknown, checks.StateDown, true},
		{"real recovery alerts", checks.StateDown, checks.StateUp, true},
		{"regression alerts", checks.StateUp, checks.StateDown, true},
		{"stale (up to unknown) suppressed", checks.StateUp, checks.StateUnknown, false},
		{"stale (down to unknown) suppressed", checks.StateDown, checks.StateUnknown, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldAlert(c.from, c.to); got != c.want {
				t.Errorf("shouldAlert(%s→%s) = %v, want %v", c.from, c.to, got, c.want)
			}
		})
	}
}

func TestParseTestState(t *testing.T) {
	cases := []struct {
		in       string
		wantFrom checks.State
		wantTo   checks.State
		wantErr  bool
	}{
		{"", checks.StateUp, checks.StateDown, false},
		{"down", checks.StateUp, checks.StateDown, false},
		{"DOWN", checks.StateUp, checks.StateDown, false},
		{"recovered", checks.StateDown, checks.StateUp, false},
		{"up", checks.StateUnknown, checks.StateUp, false},
		{"bogus", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			from, to, err := parseTestState(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if !c.wantErr && (from != c.wantFrom || to != c.wantTo) {
				t.Errorf("got (%s,%s) want (%s,%s)", from, to, c.wantFrom, c.wantTo)
			}
		})
	}
}

func TestSyntheticDetailPerType(t *testing.T) {
	cases := []struct {
		t    config.CheckType
		want string
	}{
		{config.CheckHTTP, "status 503"},
		{config.CheckTCP, "dial tcp"},
		{config.CheckICMP, "no reply"},
		{config.CheckTLS, "cert expires in 7d"},
		{config.CheckDNS, "no such host"},
	}
	for _, c := range cases {
		t.Run(string(c.t), func(t *testing.T) {
			got := syntheticDetail(&config.Check{Type: c.t, Target: "example.com"})
			if !strings.Contains(got, c.want) {
				t.Errorf("detail for %s missing %q: got %q", c.t, c.want, got)
			}
			if !strings.Contains(got, "[test]") {
				t.Errorf("detail must be tagged [test]: got %q", got)
			}
		})
	}
}

func TestSyntheticSnapshotShape(t *testing.T) {
	check := &config.Check{ID: "c1", Type: config.CheckHTTP, Target: "https://example.com"}
	down := syntheticSnapshot(check, checks.StateDown, 3)
	if down.OKCount != 0 || down.NotOK != 3 || down.Reports != 3 || down.Detail == "" {
		t.Errorf("down snapshot wrong: %+v", down)
	}
	up := syntheticSnapshot(check, checks.StateUp, 3)
	if up.OKCount != 3 || up.NotOK != 0 || up.Reports != 3 || up.Detail != "" {
		t.Errorf("up snapshot wrong: %+v", up)
	}
	// Empty cluster falls back to 1+ rather than 0 reports.
	fallback := syntheticSnapshot(check, checks.StateDown, 0)
	if fallback.Reports == 0 {
		t.Errorf("expected non-zero reports for empty-cluster fallback, got %+v", fallback)
	}
}
