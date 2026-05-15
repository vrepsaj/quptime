package alerts

// TemplateVarsHint returns a compact, multi-line listing of the
// variables a subject/body template can reference. Designed for
// embedding in TUI form hints where vertical space is tight.
//
// Continuation lines are pre-indented so they line up under the
// first line when the caller prepends a fixed indent (e.g. "  ").
func TemplateVarsHint() string {
	return "Go text/template — leave empty to use the built-in format.\n" +
		"  Vars: {{.Check.Name}}, {{.Check.Target}}, {{.Check.Type}}, {{.Check.ID}},\n" +
		"        {{.Verb}} (UP|DOWN|RECOVERED), {{.VerbLower}}, {{.From}}, {{.To}}, {{.NodeID}}, {{.When}},\n" +
		"        {{.Snapshot.Detail}}, {{.Snapshot.Reports}}, {{.Snapshot.OKCount}}, {{.Snapshot.NotOK}}"
}

// TemplateVarsHelp returns the long-form documentation for available
// template variables, suitable for embedding in a CLI command's Long
// help text. Each variable is described on its own line and an
// example template is included at the end.
func TemplateVarsHelp() string {
	return `Subject and body templates use Go text/template syntax. They are
optional — leaving them empty falls back to the built-in format.
Discord ignores the subject template (it has no subject line); SMTP
uses both.

Available variables:
  {{.Check.Name}}        check name (e.g. "homepage")
  {{.Check.Target}}      URL / host:port / host being probed
  {{.Check.Type}}        http | tcp | icmp
  {{.Check.ID}}          stable check UUID
  {{.Verb}}              UP | DOWN | RECOVERED
  {{.VerbLower}}         lowercase form of Verb (up | down | recovered)
  {{.From}}              previous state name
  {{.To}}                new state name
  {{.NodeID}}            master node that rendered the message
  {{.When}}              RFC3339 timestamp of the transition
  {{.Snapshot.Detail}}   probe detail string (e.g. "connection refused")
  {{.Snapshot.Reports}}  total reports in the flip window
  {{.Snapshot.OKCount}}  ok report count
  {{.Snapshot.NotOK}}    failing report count

Example body template:
  {{.Check.Name}} is {{.Verb}} (target {{.Check.Target}}).
  Detail: {{.Snapshot.Detail}}`
}
