package alerts

import (
	"testing"

	"git.cer.sh/axodouble/quptime/internal/checks"
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
