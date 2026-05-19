package alerts

import (
	"strings"
	"testing"

	"git.cer.sh/axodouble/quptime/internal/checks"
	"git.cer.sh/axodouble/quptime/internal/config"
)

func TestRenderDownTransition(t *testing.T) {
	check := &config.Check{Name: "homepage", Target: "https://example.com", Type: config.CheckHTTP}
	snap := checks.Snapshot{Reports: 3, OKCount: 0, NotOK: 3, Detail: "connection refused"}
	msg := Render("master-node", check, checks.StateUp, checks.StateDown, snap)

	if !strings.Contains(msg.Subject, "DOWN") {
		t.Errorf("subject missing DOWN: %q", msg.Subject)
	}
	if !strings.Contains(msg.Subject, "homepage") {
		t.Errorf("subject missing check name: %q", msg.Subject)
	}
	if !strings.Contains(msg.Body, "connection refused") {
		t.Errorf("body missing detail: %q", msg.Body)
	}
	if !strings.Contains(msg.Body, "master-node") {
		t.Errorf("body missing reporter: %q", msg.Body)
	}
	if !strings.Contains(msg.Body, "0/3 OK, 3 failing") {
		t.Errorf("body missing report count: %q", msg.Body)
	}
}

func TestRenderRecoveryTransition(t *testing.T) {
	check := &config.Check{Name: "api", Target: "https://api/", Type: config.CheckHTTP}
	snap := checks.Snapshot{Reports: 3, OKCount: 3, NotOK: 0}
	msg := Render("master", check, checks.StateDown, checks.StateUp, snap)
	if !strings.Contains(msg.Subject, "RECOVERED") {
		t.Errorf("subject missing RECOVERED: %q", msg.Subject)
	}
}

func TestRenderUpInitialTransition(t *testing.T) {
	check := &config.Check{Name: "api", Target: "https://api/"}
	snap := checks.Snapshot{Reports: 1, OKCount: 1}
	msg := Render("master", check, checks.StateUnknown, checks.StateUp, snap)
	if !strings.Contains(msg.Subject, "UP") {
		t.Errorf("subject missing UP: %q", msg.Subject)
	}
	if strings.Contains(msg.Subject, "RECOVERED") {
		t.Error("first-time UP should not be tagged RECOVERED")
	}
}

func TestRenderForUsesAlertTemplates(t *testing.T) {
	check := &config.Check{Name: "homepage", Target: "https://example.com", Type: config.CheckHTTP}
	snap := checks.Snapshot{Reports: 3, OKCount: 0, NotOK: 3, Detail: "connection refused"}
	alert := &config.Alert{
		SubjectTemplate: "{{.Check.Name}} is {{.Verb}}",
		BodyTemplate:    "{{.Check.Target}} :: {{.Snapshot.Detail}}",
	}
	msg, err := RenderFor(alert, "master", check, checks.StateUp, checks.StateDown, snap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Subject != "homepage is DOWN" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if msg.Body != "https://example.com :: connection refused" {
		t.Errorf("body = %q", msg.Body)
	}
}

func TestRenderForFallsBackToDefaultPerField(t *testing.T) {
	check := &config.Check{Name: "homepage", Target: "https://example.com", Type: config.CheckHTTP}
	snap := checks.Snapshot{Reports: 3, OKCount: 0, NotOK: 3}
	// only body overridden; subject should match default.
	alert := &config.Alert{BodyTemplate: "custom body"}
	msg, err := RenderFor(alert, "master", check, checks.StateUp, checks.StateDown, snap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(msg.Subject, "DOWN") {
		t.Errorf("subject should be default rendering, got %q", msg.Subject)
	}
	if msg.Body != "custom body" {
		t.Errorf("body = %q", msg.Body)
	}
}

func TestRenderForReportsTemplateError(t *testing.T) {
	check := &config.Check{Name: "homepage", Target: "https://example.com"}
	snap := checks.Snapshot{}
	alert := &config.Alert{BodyTemplate: "{{.Check.MissingField"} // unbalanced
	_, err := RenderFor(alert, "master", check, checks.StateUp, checks.StateDown, snap)
	if err == nil {
		t.Fatal("expected parse error for malformed template")
	}
}

func TestRenderPerTypeDefaults(t *testing.T) {
	cases := []struct {
		name           string
		check          *config.Check
		snap           checks.Snapshot
		wantSubjectHas []string
		wantBodyHas    []string
	}{
		{
			name: "http surfaces URL + expected status",
			check: &config.Check{
				Name: "homepage", Target: "https://example.com", Type: config.CheckHTTP,
				ExpectStatus: 200,
			},
			snap:           checks.Snapshot{Reports: 3, NotOK: 3, Detail: "status 503"},
			wantSubjectHas: []string{"HTTP DOWN", "homepage", "https://example.com"},
			wantBodyHas:    []string{"HTTP endpoint", `"homepage"`, "URL:", "Expected:   HTTP 200", "status 503"},
		},
		{
			name: "tls surfaces cert state + warn window",
			check: &config.Check{
				Name: "cert-watch", Target: "example.com", Type: config.CheckTLS,
				TLSWarnDays: 14, TLSServerName: "api.example.com",
			},
			snap:           checks.Snapshot{Reports: 3, NotOK: 3, Detail: "cert expires in 7d (notAfter=2026-05-26T...)"},
			wantSubjectHas: []string{"TLS cert DOWN", "cert-watch", "example.com"},
			wantBodyHas:    []string{"TLS certificate for", "Host:", "SNI:", "Warn window: 14d", "Cert state:", "cert expires in 7d"},
		},
		{
			name:           "tcp focuses on endpoint",
			check:          &config.Check{Name: "db", Target: "db.internal:5432", Type: config.CheckTCP},
			snap:           checks.Snapshot{Reports: 3, NotOK: 3, Detail: "dial tcp: i/o timeout"},
			wantSubjectHas: []string{"TCP DOWN", "db", "db.internal:5432"},
			wantBodyHas:    []string{"TCP service", "Endpoint:   db.internal:5432", "dial tcp"},
		},
		{
			name:           "icmp focuses on host",
			check:          &config.Check{Name: "gateway", Target: "10.0.0.1", Type: config.CheckICMP},
			snap:           checks.Snapshot{Reports: 3, NotOK: 3, Detail: "no reply"},
			wantSubjectHas: []string{"Ping DOWN", "gateway", "10.0.0.1"},
			wantBodyHas:    []string{"Host", "10.0.0.1", "no reply"},
		},
		{
			name: "dns surfaces record + resolver + expected substring",
			check: &config.Check{
				Name: "apex", Target: "example.com", Type: config.CheckDNS,
				DNSRecord: "a", DNSResolver: "1.1.1.1:53", DNSExpect: "93.184.",
			},
			snap:           checks.Snapshot{Reports: 3, NotOK: 3, Detail: "lookup example.com: no such host"},
			wantSubjectHas: []string{"DNS DOWN", "apex", "example.com"},
			wantBodyHas:    []string{"DNS lookup for", "Record:     a", "Resolver:   1.1.1.1:53", `Expected:   contains "93.184."`, "no such host"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := Render("master-node", c.check, checks.StateUp, checks.StateDown, c.snap)
			for _, want := range c.wantSubjectHas {
				if !strings.Contains(msg.Subject, want) {
					t.Errorf("subject missing %q: %q", want, msg.Subject)
				}
			}
			for _, want := range c.wantBodyHas {
				if !strings.Contains(msg.Body, want) {
					t.Errorf("body missing %q in:\n%s", want, msg.Body)
				}
			}
		})
	}
}

func TestRenderPerTypeOptionalFieldsOmitted(t *testing.T) {
	// HTTP without ExpectStatus or BodyMatch should not emit those labels.
	http := Render("m", &config.Check{Name: "h", Target: "https://x", Type: config.CheckHTTP}, checks.StateUp, checks.StateDown, checks.Snapshot{Reports: 1, NotOK: 1})
	if strings.Contains(http.Body, "Expected:") {
		t.Errorf("http body should not mention Expected when ExpectStatus is zero: %q", http.Body)
	}
	if strings.Contains(http.Body, "Body match:") {
		t.Errorf("http body should not mention Body match when BodyMatch is empty: %q", http.Body)
	}
	// DNS without record/resolver/expect should not emit those labels.
	dns := Render("m", &config.Check{Name: "d", Target: "x.com", Type: config.CheckDNS}, checks.StateUp, checks.StateDown, checks.Snapshot{Reports: 1, NotOK: 1})
	for _, label := range []string{"Record:", "Resolver:", "Expected:"} {
		if strings.Contains(dns.Body, label) {
			t.Errorf("dns body should not mention %s with no config: %q", label, dns.Body)
		}
	}
}

func TestDefaultTemplateAccessor(t *testing.T) {
	cases := []struct {
		t            config.CheckType
		wantSubject  string
		wantBody     string
	}{
		{config.CheckHTTP, DefaultSubjectHTTP, DefaultBodyHTTP},
		{config.CheckTLS, DefaultSubjectTLS, DefaultBodyTLS},
		{config.CheckTCP, DefaultSubjectTCP, DefaultBodyTCP},
		{config.CheckICMP, DefaultSubjectICMP, DefaultBodyICMP},
		{config.CheckDNS, DefaultSubjectDNS, DefaultBodyDNS},
		{config.CheckType("unknown-future-kind"), DefaultSubjectGeneric, DefaultBodyGeneric},
	}
	for _, c := range cases {
		t.Run(string(c.t), func(t *testing.T) {
			gotS, gotB := DefaultTemplate(c.t)
			if gotS != c.wantSubject {
				t.Errorf("subject mismatch for %s", c.t)
			}
			if gotB != c.wantBody {
				t.Errorf("body mismatch for %s", c.t)
			}
		})
	}
}

func TestRenderForNilAlertReturnsDefault(t *testing.T) {
	check := &config.Check{Name: "homepage", Target: "https://example.com"}
	snap := checks.Snapshot{Reports: 1, OKCount: 1}
	msg, err := RenderFor(nil, "master", check, checks.StateUp, checks.StateUp, snap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(msg.Subject, "homepage") {
		t.Errorf("default subject should mention check, got %q", msg.Subject)
	}
}
