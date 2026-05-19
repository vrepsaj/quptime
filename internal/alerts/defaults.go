package alerts

import (
	"text/template"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// Per-type default subject and body templates. These are used when an
// alert does not configure its own SubjectTemplate / BodyTemplate.
//
// They are Go text/template syntax and are exported so the
// documentation can include verbatim copies and operators can paste
// one into an alert as a starting point for customisation.
//
// Each template surfaces the fields that matter for that probe type:
// HTTP highlights URL + expected status, TLS highlights cert state +
// warn window, DNS highlights record type / resolver / expected
// substring, and TCP / ICMP keep a minimal connectivity-focused shape.

const (
	DefaultSubjectHTTP = `[quptime] HTTP {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})`

	DefaultBodyHTTP = `HTTP endpoint "{{.Check.Name}}" is now {{.VerbLower}}.

URL:        {{.Check.Target}}
{{- if .Check.ExpectStatus}}
Expected:   HTTP {{.Check.ExpectStatus}}
{{- end}}
{{- if .Check.BodyMatch}}
Body match: contains "{{.Check.BodyMatch}}"
{{- end}}
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
`

	DefaultSubjectTLS = `[quptime] TLS cert {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})`

	DefaultBodyTLS = `TLS certificate for "{{.Check.Name}}" is now {{.VerbLower}}.

Host:        {{.Check.Target}}
{{- if .Check.TLSServerName}}
SNI:         {{.Check.TLSServerName}}
{{- end}}
{{- if .Check.TLSWarnDays}}
Warn window: {{.Check.TLSWarnDays}}d before NotAfter
{{- end}}
{{- if .Snapshot.Detail}}
Cert state:  {{.Snapshot.Detail}}
{{- end}}
Previous:    {{.From}}
Reporters:   {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:      {{.NodeID}}
When:        {{.When}}
`

	DefaultSubjectTCP = `[quptime] TCP {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})`

	DefaultBodyTCP = `TCP service "{{.Check.Name}}" is now {{.VerbLower}}.

Endpoint:   {{.Check.Target}}
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
`

	DefaultSubjectICMP = `[quptime] Ping {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})`

	DefaultBodyICMP = `Host "{{.Check.Name}}" is now {{.VerbLower}}.

Host:       {{.Check.Target}}
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
`

	DefaultSubjectDNS = `[quptime] DNS {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})`

	DefaultBodyDNS = `DNS lookup for "{{.Check.Name}}" is now {{.VerbLower}}.

Target:     {{.Check.Target}}
{{- if .Check.DNSRecord}}
Record:     {{.Check.DNSRecord}}
{{- end}}
{{- if .Check.DNSResolver}}
Resolver:   {{.Check.DNSResolver}}
{{- end}}
{{- if .Check.DNSExpect}}
Expected:   contains "{{.Check.DNSExpect}}"
{{- end}}
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
`

	// DefaultSubjectGeneric / DefaultBodyGeneric are the catch-all used
	// for any future CheckType that doesn't yet have a dedicated
	// template. Should never be reached today given the enum.
	DefaultSubjectGeneric = `[quptime] {{.Check.Name}} {{.Verb}} — {{.Check.Target}}`

	DefaultBodyGeneric = `Check "{{.Check.Name}}" is now {{.VerbLower}}.

Target:     {{.Check.Target}} ({{.Check.Type}})
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
`
)

// Pre-parsed forms keyed by CheckType, populated at init. Parsing a
// constant template never fails at runtime, so template.Must is the
// right tool — any breakage is a programmer error caught on startup.
var (
	parsedDefaultSubjects = map[config.CheckType]*template.Template{}
	parsedDefaultBodies   = map[config.CheckType]*template.Template{}
)

func init() {
	defaults := []struct {
		t       config.CheckType
		subject string
		body    string
	}{
		{config.CheckHTTP, DefaultSubjectHTTP, DefaultBodyHTTP},
		{config.CheckTLS, DefaultSubjectTLS, DefaultBodyTLS},
		{config.CheckTCP, DefaultSubjectTCP, DefaultBodyTCP},
		{config.CheckICMP, DefaultSubjectICMP, DefaultBodyICMP},
		{config.CheckDNS, DefaultSubjectDNS, DefaultBodyDNS},
	}
	for _, d := range defaults {
		parsedDefaultSubjects[d.t] = template.Must(template.New("subj-" + string(d.t)).Option("missingkey=zero").Parse(d.subject))
		parsedDefaultBodies[d.t] = template.Must(template.New("body-" + string(d.t)).Option("missingkey=zero").Parse(d.body))
	}
	parsedDefaultSubjects[""] = template.Must(template.New("subj-generic").Option("missingkey=zero").Parse(DefaultSubjectGeneric))
	parsedDefaultBodies[""] = template.Must(template.New("body-generic").Option("missingkey=zero").Parse(DefaultBodyGeneric))
}

// DefaultTemplate returns the raw (unparsed) subject and body
// templates for the given check type, so callers can copy them into
// custom alert configuration as a starting point.
func DefaultTemplate(t config.CheckType) (subject, body string) {
	switch t {
	case config.CheckHTTP:
		return DefaultSubjectHTTP, DefaultBodyHTTP
	case config.CheckTLS:
		return DefaultSubjectTLS, DefaultBodyTLS
	case config.CheckTCP:
		return DefaultSubjectTCP, DefaultBodyTCP
	case config.CheckICMP:
		return DefaultSubjectICMP, DefaultBodyICMP
	case config.CheckDNS:
		return DefaultSubjectDNS, DefaultBodyDNS
	}
	return DefaultSubjectGeneric, DefaultBodyGeneric
}

// defaultTemplateFor returns the parsed subject + body templates for
// the given check type, falling back to the generic templates for an
// unknown type. Used by Render.
func defaultTemplateFor(t config.CheckType) (*template.Template, *template.Template) {
	subj, ok := parsedDefaultSubjects[t]
	if !ok {
		subj = parsedDefaultSubjects[""]
	}
	body, ok := parsedDefaultBodies[t]
	if !ok {
		body = parsedDefaultBodies[""]
	}
	return subj, body
}
