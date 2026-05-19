// Package alerts dispatches state-transition notifications to the
// configured channels (SMTP, Discord). The aggregator owns hysteresis
// so this package fires exactly one message per UP↔DOWN flip.
package alerts

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"

	"git.cer.sh/axodouble/quptime/internal/checks"
	"git.cer.sh/axodouble/quptime/internal/config"
)

// TemplateContext is what user-provided subject/body templates see. It
// is also the shape the default renderer fills in, so changing one
// place keeps the two paths consistent.
type TemplateContext struct {
	Check    *config.Check
	From     string          // previous state name
	To       string          // new state name
	Verb     string          // "UP" | "DOWN" | "RECOVERED"
	VerbLower string         // lowercase form of Verb ("up" | "down" | "recovered")
	Snapshot checks.Snapshot // aggregate counts and detail
	NodeID   string          // master that rendered the message
	When     string          // RFC3339 timestamp
}

// Message is the rendered notification ready to ship across any
// channel. Channels may format Subject + Body differently (SMTP uses
// both; Discord renders a single string).
type Message struct {
	Subject string
	Body    string
}

// Render produces a human-readable message from one state transition
// using the built-in per-type format. Used as the fallback when no
// custom template is configured (or when a custom template fails to
// render). The exact wording varies by check.Type so HTTP, TLS, TCP,
// ICMP, and DNS alerts each surface the fields that matter for that
// probe — see defaults.go for the templates themselves.
func Render(nodeID string, check *config.Check, from, to checks.State, snap checks.Snapshot) Message {
	ctx := newContext(nodeID, check, from, to, snap)
	subjTmpl, bodyTmpl := defaultTemplateFor(check.Type)
	var sb, bb bytes.Buffer
	if err := subjTmpl.Execute(&sb, ctx); err != nil {
		sb.Reset()
		fmt.Fprintf(&sb, "[quptime] %s %s — %s", check.Name, ctx.Verb, check.Target)
	}
	if err := bodyTmpl.Execute(&bb, ctx); err != nil {
		bb.Reset()
		fmt.Fprintf(&bb, "Check %q is now %s. Detail: %s\n", check.Name, ctx.Verb, snap.Detail)
	}
	return Message{Subject: sb.String(), Body: bb.String()}
}

// RenderFor produces a message for one specific alert. If the alert
// defines SubjectTemplate or BodyTemplate, those override the
// corresponding field from the default render. A template error falls
// back to the default for that field and is reported via the returned
// error (the caller is expected to log but still ship the message).
func RenderFor(alert *config.Alert, nodeID string, check *config.Check, from, to checks.State, snap checks.Snapshot) (Message, error) {
	def := Render(nodeID, check, from, to, snap)
	if alert == nil || (alert.SubjectTemplate == "" && alert.BodyTemplate == "") {
		return def, nil
	}
	ctx := newContext(nodeID, check, from, to, snap)
	msg := def
	var firstErr error
	if alert.SubjectTemplate != "" {
		s, err := execTemplate("subject", alert.SubjectTemplate, ctx)
		if err != nil {
			firstErr = err
		} else {
			msg.Subject = s
		}
	}
	if alert.BodyTemplate != "" {
		s, err := execTemplate("body", alert.BodyTemplate, ctx)
		if err != nil && firstErr == nil {
			firstErr = err
		} else if err == nil {
			msg.Body = s
		}
	}
	return msg, firstErr
}

func newContext(nodeID string, check *config.Check, from, to checks.State, snap checks.Snapshot) TemplateContext {
	verb := transitionVerb(from, to)
	return TemplateContext{
		Check:     check,
		From:      string(from),
		To:        string(to),
		Verb:      verb,
		VerbLower: strings.ToLower(verb),
		Snapshot:  snap,
		NodeID:    nodeID,
		When:      time.Now().UTC().Format(time.RFC3339),
	}
}

func execTemplate(name, src string, ctx TemplateContext) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=zero").Parse(src)
	if err != nil {
		return "", fmt.Errorf("parse %s template: %w", name, err)
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, ctx); err != nil {
		return "", fmt.Errorf("execute %s template: %w", name, err)
	}
	return b.String(), nil
}

func transitionVerb(from, to checks.State) string {
	switch to {
	case checks.StateDown:
		return "DOWN"
	case checks.StateUp:
		if from == checks.StateDown {
			return "RECOVERED"
		}
		return "UP"
	}
	return strings.ToUpper(string(to))
}
