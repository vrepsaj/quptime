package alerts

import (
	"fmt"
	"log"
	"strings"
	"time"

	"git.cer.sh/axodouble/quptime/internal/checks"
	"git.cer.sh/axodouble/quptime/internal/config"
)

// Dispatcher fans an aggregator transition out to every alert listed
// on the check. Errors are logged but never propagated: alerting must
// not block the aggregation pipeline.
type Dispatcher struct {
	cluster *config.ClusterConfig
	selfID  string
	logger  *log.Logger
}

// New constructs a Dispatcher.
func New(cluster *config.ClusterConfig, selfID string, logger *log.Logger) *Dispatcher {
	if logger == nil {
		logger = log.Default()
	}
	return &Dispatcher{cluster: cluster, selfID: selfID, logger: logger}
}

// OnTransition is wired as checks.TransitionFn.
func (d *Dispatcher) OnTransition(check *config.Check, from, to checks.State, snap checks.Snapshot) {
	if !shouldAlert(from, to) {
		return
	}
	alerts := d.cluster.EffectiveAlertsFor(check)
	if len(alerts) == 0 && len(check.AlertIDs) > 0 {
		d.logger.Printf("alerts: check %q references alerts but none resolved", check.Name)
	}
	for i := range alerts {
		alert := alerts[i]
		msg, err := RenderFor(&alert, d.selfID, check, from, to, snap)
		if err != nil {
			d.logger.Printf("alerts: %q template: %v — falling back to default", alert.Name, err)
		}
		if err := d.dispatchOne(&alert, msg); err != nil {
			d.logger.Printf("alerts: %q via %s: %v", alert.Name, alert.Type, err)
		}
	}
}

// TestCheck fires a synthetic transition for a real check through
// every alert that would actually receive it (Default + check.AlertIDs
// minus SuppressAlertIDs), so an operator can validate end-to-end
// message formatting — including type-specific Detail rendering and
// any custom templates — without waiting for a real outage. The
// hysteresis-gated `shouldAlert` filter is intentionally bypassed
// here: this is the operator explicitly asking for the message.
//
// state controls the synthetic transition shape:
//   - "down"      → from=Up,      to=Down   (verb=DOWN)
//   - "recovered" → from=Down,    to=Up     (verb=RECOVERED)
//   - "up"        → from=Unknown, to=Up     (verb=UP)
//
// The synthetic Snapshot reports N OK/Fail matching the configured
// nodes (best-effort — falls back to 3 if the cluster is empty),
// and Detail is a type-aware placeholder so cert-expiry / DNS / HTTP
// templates render something that looks like a real failure.
func (d *Dispatcher) TestCheck(checkID, state string) error {
	snap := d.cluster.Snapshot()
	var check *config.Check
	for i := range snap.Checks {
		if snap.Checks[i].ID == checkID || snap.Checks[i].Name == checkID {
			cp := snap.Checks[i]
			check = &cp
			break
		}
	}
	if check == nil {
		return fmt.Errorf("check %q not found", checkID)
	}

	from, to, err := parseTestState(state)
	if err != nil {
		return err
	}

	nodes := len(snap.Peers)
	if nodes == 0 {
		nodes = 3
	}
	ss := syntheticSnapshot(check, to, nodes)

	alerts := d.cluster.EffectiveAlertsFor(check)
	if len(alerts) == 0 {
		return fmt.Errorf("check %q has no effective alerts (nothing to fire)", check.Name)
	}

	var errs []string
	for i := range alerts {
		alert := alerts[i]
		msg, rerr := RenderFor(&alert, d.selfID, check, from, to, ss)
		if rerr != nil {
			d.logger.Printf("alerts: %q template: %v — falling back to default", alert.Name, rerr)
		}
		if derr := d.dispatchOne(&alert, msg); derr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", alert.Name, derr))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d/%d alerts failed: %s", len(errs), len(alerts), strings.Join(errs, "; "))
	}
	return nil
}

// parseTestState normalises the operator-supplied transition keyword
// into a from/to pair. Empty / unknown values default to a DOWN test
// (the most useful one when validating a brand-new alert template).
func parseTestState(state string) (checks.State, checks.State, error) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "down":
		return checks.StateUp, checks.StateDown, nil
	case "recovered":
		return checks.StateDown, checks.StateUp, nil
	case "up":
		return checks.StateUnknown, checks.StateUp, nil
	default:
		return "", "", fmt.Errorf("unknown test state %q (want down|up|recovered)", state)
	}
}

// syntheticSnapshot builds a plausible aggregate snapshot for a test
// transition. Down → all nodes report failure with a type-specific
// detail string; Up → all nodes report OK with no detail.
func syntheticSnapshot(check *config.Check, to checks.State, nodes int) checks.Snapshot {
	if nodes < 1 {
		nodes = 1
	}
	if to == checks.StateDown {
		return checks.Snapshot{
			CheckID: check.ID,
			State:   to,
			Reports: nodes,
			OKCount: 0,
			NotOK:   nodes,
			Detail:  syntheticDetail(check),
		}
	}
	return checks.Snapshot{
		CheckID: check.ID,
		State:   to,
		Reports: nodes,
		OKCount: nodes,
		NotOK:   0,
	}
}

// syntheticDetail returns a plausible-looking failure message for
// each check type so an operator testing their templates sees
// something representative of a real outage. The "[test]" prefix
// keeps it honest — nobody is fooled into thinking this is real.
func syntheticDetail(check *config.Check) string {
	switch check.Type {
	case config.CheckHTTP:
		return "[test] status 503 Service Unavailable"
	case config.CheckTCP:
		return "[test] dial tcp: i/o timeout"
	case config.CheckICMP:
		return "[test] no reply"
	case config.CheckTLS:
		return "[test] cert expires in 7d (notAfter=" + time.Now().Add(7*24*time.Hour).UTC().Format(time.RFC3339) + ")"
	case config.CheckDNS:
		return "[test] lookup " + check.Target + ": no such host"
	}
	return "[test] synthetic failure"
}

// Test sends a one-shot test message to the named alert. Returns an
// error so the CLI can surface failures interactively. If the alert
// carries custom templates they are exercised against a synthetic
// "homepage going DOWN" transition so the operator can confirm the
// template renders before a real outage.
func (d *Dispatcher) Test(alertID string) error {
	alert := d.cluster.FindAlert(alertID)
	if alert == nil {
		return fmt.Errorf("alert %q not found", alertID)
	}
	if alert.SubjectTemplate == "" && alert.BodyTemplate == "" {
		msg := Message{
			Subject: "[quptime] test alert",
			Body:    fmt.Sprintf("This is a test of alert %q from node %s.\nIf you see this, the alert channel is wired correctly.\n", alert.Name, d.selfID),
		}
		return d.dispatchOne(alert, msg)
	}
	sample := &config.Check{
		ID:     "test-check",
		Name:   "test-check",
		Type:   config.CheckHTTP,
		Target: "https://example.com",
	}
	snap := checks.Snapshot{Reports: 3, OKCount: 0, NotOK: 3, Detail: "synthetic test failure"}
	msg, err := RenderFor(alert, d.selfID, sample, checks.StateUp, checks.StateDown, snap)
	if err != nil {
		return fmt.Errorf("render template: %w", err)
	}
	return d.dispatchOne(alert, msg)
}

// shouldAlert decides whether a committed state transition warrants
// firing the configured alert channels.
//
// A fresh master's aggregator starts every check at StateUnknown, so
// the first successful evaluation always commits Unknown→Up. Without
// filtering, every master failover (or daemon restart) would spam an
// "is now UP" alert for every healthy check. We treat Unknown→Up as a
// silent cold start; real recoveries (Down→Up) and any transition to
// Down still alert.
func shouldAlert(from, to checks.State) bool {
	if to == checks.StateUnknown {
		return false
	}
	if from == checks.StateUnknown && to == checks.StateUp {
		return false
	}
	return true
}

func (d *Dispatcher) dispatchOne(a *config.Alert, msg Message) error {
	switch a.Type {
	case config.AlertSMTP:
		return sendSMTP(a, msg)
	case config.AlertDiscord:
		return sendDiscord(a, msg)
	default:
		return fmt.Errorf("unknown alert type %q", a.Type)
	}
}
