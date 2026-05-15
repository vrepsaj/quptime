package alerts

import (
	"fmt"
	"log"

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
