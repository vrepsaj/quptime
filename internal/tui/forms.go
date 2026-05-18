package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/google/uuid"

	"git.cer.sh/axodouble/quptime/internal/alerts"
	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

// modalDone tells the parent the modal is finished. Flash, when set,
// is shown as a one-shot status line; level controls the color.
type modalDone struct {
	flash string
	level flashLevel
}

type flashLevel int

const (
	flashInfo flashLevel = iota
	flashWarn
	flashError
)

// modal is implemented by every pop-up form/dialog. Parent passes all
// input to the modal's Update; when the modal completes it returns
// modalDone as a tea.Msg via its tea.Cmd.
//
// Init is called once when a modal is installed (or replaced via
// modal → modal handoff in a picker). Return any startup Cmd here —
// most modals return nil, but forms return the blink Cmd that drives
// the cursor animation on the initially focused field.
type modal interface {
	Init() tea.Cmd
	Update(tea.Msg) (modal, tea.Cmd)
	View() string
	Title() string
}

func modalDoneCmd(flash string, level flashLevel) tea.Cmd {
	return func() tea.Msg { return modalDone{flash: flash, level: level} }
}

// =============================================================
// Generic field-based form (used by check/alert/node add flows).
// =============================================================

type formField struct {
	label     string
	input     textinput.Model
	textarea  textarea.Model
	multiline bool
	required  bool
	hint      string
}

// value returns the field's current text regardless of whether it's
// backed by a single-line input or a multiline textarea.
func (fld *formField) value() string {
	if fld.multiline {
		return fld.textarea.Value()
	}
	return fld.input.Value()
}

// focus returns the input's blink Cmd. v2 textinput/textarea drive a
// blinking cursor via this cmd — discard it and the cursor sits idle
// until the user types.
func (fld *formField) focus() tea.Cmd {
	if fld.multiline {
		return fld.textarea.Focus()
	}
	return fld.input.Focus()
}

func (fld *formField) blur() {
	if fld.multiline {
		fld.textarea.Blur()
		return
	}
	fld.input.Blur()
}

func (fld *formField) setWidth(w int) {
	if fld.multiline {
		fld.textarea.SetWidth(w)
		return
	}
	fld.input.SetWidth(w)
}

type form struct {
	title  string
	fields []formField
	cursor int
	busy   bool
	err    string
	width  int // current terminal width; inputs resize to fill it

	// initCmd is the blink Cmd produced by focusing the first field at
	// construction time. The parent dispatches it via Init() so the
	// cursor starts blinking the moment the form appears.
	initCmd tea.Cmd

	submit func(values []string) tea.Cmd
}

// defaultFieldWidth is the fallback input width used before the first
// WindowSizeMsg has arrived. Once we know the terminal size, inputs
// grow to fill the available horizontal space.
const defaultFieldWidth = 40

// fieldWidthFor derives the per-input visible width from the terminal
// width. It subtracts the modal's border+padding (6) and the form's
// label indent (2), then a couple of chars of safety margin.
func fieldWidthFor(termWidth int) int {
	w := termWidth - 12
	if w < defaultFieldWidth {
		return defaultFieldWidth
	}
	return w
}

func newForm(title string, fields []formField, submit func([]string) tea.Cmd) *form {
	var initCmd tea.Cmd
	for i := range fields {
		if !fields[i].multiline {
			fields[i].input.Prompt = ""
			fields[i].input.CharLimit = 256
		}
		if i == 0 {
			initCmd = fields[i].focus()
		} else {
			fields[i].blur()
		}
	}
	return &form{title: title, fields: fields, submit: submit, initCmd: initCmd}
}

func textField(label, hint string, required bool) formField {
	return textFieldWithValue(label, hint, "", required)
}

// textFieldWithValue is the same as textField but pre-populates the
// input with `value`. Used by edit forms so the user sees the current
// contents and can tweak instead of retyping everything.
func textFieldWithValue(label, hint, value string, required bool) formField {
	ti := textinput.New()
	ti.SetWidth(defaultFieldWidth)
	ti.Placeholder = hint
	if value != "" {
		ti.SetValue(value)
	}
	return formField{label: label, hint: hint, required: required, input: ti}
}

// textAreaField creates a multiline field. Enter inserts a newline;
// the form uses shift+enter / ctrl+s to submit when the cursor is on
// one of these. Useful for things like alert body templates where the
// rendered message naturally spans multiple lines.
func textAreaField(label, hint string, required bool) formField {
	return textAreaFieldWithValue(label, hint, "", required)
}

func textAreaFieldWithValue(label, hint, value string, required bool) formField {
	ta := textarea.New()
	ta.Placeholder = hint
	ta.ShowLineNumbers = false
	ta.Prompt = "  "
	ta.SetHeight(5)
	ta.SetWidth(defaultFieldWidth)
	ta.CharLimit = 0
	// Keep enter bound to "insert newline" (the textarea default) — the
	// surrounding form intercepts enter on single-line fields and handles
	// shift+enter/ctrl+s as the submit/advance trigger for multiline ones.
	if value != "" {
		ta.SetValue(value)
	}
	return formField{label: label, hint: hint, required: required, multiline: true, textarea: ta}
}

func passwordField(label, hint string) formField {
	return passwordFieldWithValue(label, hint, "")
}

// passwordFieldWithValue pre-populates the masked input. Mostly useful
// for edit forms — the user sees that *something* is set (dots) without
// the actual value leaking on-screen.
func passwordFieldWithValue(label, hint, value string) formField {
	ti := textinput.New()
	ti.SetWidth(defaultFieldWidth)
	ti.Placeholder = hint
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	if value != "" {
		ti.SetValue(value)
	}
	return formField{label: label, hint: hint, input: ti}
}

func (f *form) Title() string { return f.title }

func (f *form) Init() tea.Cmd { return f.initCmd }

func (f *form) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", titleStyle.Render(f.title))
	for i, fld := range f.fields {
		marker := "  "
		labelStyle := subtleStyle
		if i == f.cursor {
			marker = "▸ "
			labelStyle = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
		}
		fmt.Fprintf(&b, "%s%s\n", marker, labelStyle.Render(fld.label))
		if fld.multiline {
			fmt.Fprintf(&b, "%s\n", fld.textarea.View())
		} else {
			fmt.Fprintf(&b, "  %s\n", fld.input.View())
		}
		if i == f.cursor && fld.hint != "" {
			fmt.Fprintf(&b, "  %s\n", helpStyle.Render(fld.hint))
		}
		b.WriteByte('\n')
	}
	if f.err != "" {
		fmt.Fprintf(&b, "%s\n\n", flashErrorStyle.Render("error: "+f.err))
	}
	if f.busy {
		fmt.Fprintf(&b, "%s\n", flashWarnStyle.Render("working…"))
	} else {
		help := "↑↓ field   enter next/submit   esc cancel"
		if f.cursor < len(f.fields) && f.fields[f.cursor].multiline {
			help = "tab field   enter newline   shift+enter/ctrl+s submit   esc cancel"
		}
		fmt.Fprintf(&b, "%s\n", helpStyle.Render(help))
	}
	return b.String()
}

func (f *form) Update(msg tea.Msg) (modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		f.width = msg.Width
		w := fieldWidthFor(msg.Width)
		for i := range f.fields {
			f.fields[i].setWidth(w)
		}
		return f, nil

	case formSubmitErr:
		f.busy = false
		f.err = string(msg)
		return f, nil

	case tea.KeyMsg:
		key := msg.String()
		// up/down on a multiline field belong to in-text navigation;
		// leave field-switching to tab/shift+tab there. Same for enter:
		// the textarea owns it as "insert newline", so submission moves
		// to shift+enter / ctrl+s.
		multiline := f.cursor < len(f.fields) && f.fields[f.cursor].multiline
		switch key {
		case "esc":
			return f, modalDoneCmd("", flashInfo)
		case "tab":
			return f, f.advance(1)
		case "shift+tab":
			return f, f.advance(-1)
		case "down":
			if !multiline {
				return f, f.advance(1)
			}
		case "up":
			if !multiline {
				return f, f.advance(-1)
			}
		case "enter":
			if !multiline {
				return f, f.submitOrAdvance()
			}
		case "shift+enter", "ctrl+s":
			return f, f.submitOrAdvance()
		}
	}
	var cmd tea.Cmd
	if f.fields[f.cursor].multiline {
		f.fields[f.cursor].textarea, cmd = f.fields[f.cursor].textarea.Update(msg)
	} else {
		f.fields[f.cursor].input, cmd = f.fields[f.cursor].input.Update(msg)
	}
	return f, cmd
}

// submitOrAdvance is the shared trigger for enter on single-line fields
// and shift+enter / ctrl+s on multiline fields: jump to the next field
// or, on the last one, validate and run submit.
func (f *form) submitOrAdvance() tea.Cmd {
	if f.busy {
		return nil
	}
	if f.cursor < len(f.fields)-1 {
		return f.advance(1)
	}
	vals := make([]string, len(f.fields))
	for i := range f.fields {
		vals[i] = f.fields[i].value()
	}
	for i, fld := range f.fields {
		if fld.required && strings.TrimSpace(vals[i]) == "" {
			f.err = fld.label + " is required"
			f.cursor = i
			return f.focusOnly(i)
		}
	}
	f.busy = true
	f.err = ""
	return f.submit(vals)
}

// advance moves the cursor by delta and returns the focus-blink Cmd
// from the newly focused field so the parent can dispatch it.
func (f *form) advance(delta int) tea.Cmd {
	n := len(f.fields)
	if n == 0 {
		return nil
	}
	f.cursor = (f.cursor + delta + n) % n
	return f.focusOnly(f.cursor)
}

func (f *form) focusOnly(i int) tea.Cmd {
	var cmd tea.Cmd
	for j := range f.fields {
		if j == i {
			cmd = f.fields[j].focus()
		} else {
			f.fields[j].blur()
		}
	}
	return cmd
}

// formSubmitErr is a tea.Msg the submit cmd returns to surface an
// error inline without closing the form.
type formSubmitErr string

// =============================================================
// Specific forms.
// =============================================================

func newAddCheckForm(checkType config.CheckType) *form {
	fields := []formField{
		textField("Name", "human-friendly identifier", true),
		textField("Target", targetHint(checkType), true),
		textField("Interval", "e.g. 30s, 1m", false),
		textField("Timeout", "e.g. 10s", false),
		textField("Alerts", "comma-separated alert IDs/names (optional)", false),
	}
	switch checkType {
	case config.CheckHTTP:
		fields = append(fields,
			textField("Expect status", "e.g. 200 (HTTP only)", false),
			textField("Body match", "substring required (HTTP only)", false),
		)
	case config.CheckTLS:
		fields = append(fields,
			textField("Warn days", "fail when cert expires within N days (default 14)", false),
			textField("SNI", "override server name; blank = host from target", false),
		)
	case config.CheckDNS:
		fields = append(fields,
			textField("Record", "a | aaaa | cname | mx | txt | ns (default a)", false),
			textField("Resolver", "host:port; blank = system resolver", false),
			textField("Expect", "substring required in an answer (optional)", false),
		)
	}
	return newForm("Add "+strings.ToUpper(string(checkType))+" check", fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			ch := config.Check{
				ID:       uuid.NewString(),
				Name:     strings.TrimSpace(vals[0]),
				Type:     checkType,
				Target:   strings.TrimSpace(vals[1]),
				Interval: parseDurationOr(vals[2], 30*time.Second),
				Timeout:  parseDurationOr(vals[3], 10*time.Second),
			}
			if a := strings.TrimSpace(vals[4]); a != "" {
				for _, p := range strings.Split(a, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						ch.AlertIDs = append(ch.AlertIDs, p)
					}
				}
			}
			switch checkType {
			case config.CheckHTTP:
				ch.ExpectStatus = atoiOr(vals[5], 200)
				ch.BodyMatch = strings.TrimSpace(vals[6])
			case config.CheckTLS:
				ch.TLSWarnDays = atoiOr(vals[5], 14)
				ch.TLSServerName = strings.TrimSpace(vals[6])
			case config.CheckDNS:
				ch.DNSRecord = strings.ToLower(strings.TrimSpace(vals[5]))
				ch.DNSResolver = strings.TrimSpace(vals[6])
				ch.DNSExpect = strings.TrimSpace(vals[7])
			}
			if err := mutateAdd(transport.MutationAddCheck, ch); err != nil {
				return formSubmitErr(err.Error())
			}
			return modalDone{flash: "added check " + ch.Name, level: flashInfo}
		}
	})
}

func newAddDiscordForm() *form {
	fields := []formField{
		textField("Name", "human-friendly identifier", true),
		textField("Webhook URL", "https://discord.com/api/webhooks/...", true),
		textField("Default", "yes/no — attach to every check automatically", false),
		textAreaField("Body template", alerts.TemplateVarsHint(), false),
	}
	return newForm("Add Discord alert", fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			a := config.Alert{
				ID:             uuid.NewString(),
				Name:           strings.TrimSpace(vals[0]),
				Type:           config.AlertDiscord,
				DiscordWebhook: strings.TrimSpace(vals[1]),
				Default:        parseBool(vals[2]),
				BodyTemplate:   vals[3],
			}
			if err := mutateAdd(transport.MutationAddAlert, a); err != nil {
				return formSubmitErr(err.Error())
			}
			return modalDone{flash: "added discord alert " + a.Name, level: flashInfo}
		}
	})
}

func newAddSMTPForm() *form {
	fields := []formField{
		textField("Name", "human-friendly identifier", true),
		textField("Host", "smtp.example.com", true),
		textField("Port", "default 587", false),
		textField("User", "leave empty for anonymous", false),
		passwordField("Password", "smtp auth password"),
		textField("From", "envelope From address", true),
		textField("To", "comma-separated recipient addresses", true),
		textField("StartTLS", "yes/no — default yes", false),
		textField("Default", "yes/no — attach to every check", false),
		textField("Subject template", alerts.TemplateVarsHint(), false),
		textAreaField("Body template", alerts.TemplateVarsHint(), false),
	}
	return newForm("Add SMTP alert", fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			to := strings.Split(strings.TrimSpace(vals[6]), ",")
			for i := range to {
				to[i] = strings.TrimSpace(to[i])
			}
			a := config.Alert{
				ID:              uuid.NewString(),
				Name:            strings.TrimSpace(vals[0]),
				Type:            config.AlertSMTP,
				SMTPHost:        strings.TrimSpace(vals[1]),
				SMTPPort:        atoiOr(vals[2], 587),
				SMTPUser:        strings.TrimSpace(vals[3]),
				SMTPPassword:    vals[4],
				SMTPFrom:        strings.TrimSpace(vals[5]),
				SMTPTo:          to,
				SMTPStartTLS:    parseBoolOr(vals[7], true),
				Default:         parseBool(vals[8]),
				SubjectTemplate: vals[9],
				BodyTemplate:    vals[10],
			}
			if err := mutateAdd(transport.MutationAddAlert, a); err != nil {
				return formSubmitErr(err.Error())
			}
			return modalDone{flash: "added smtp alert " + a.Name, level: flashInfo}
		}
	})
}

// newAddNodeForm mints a fresh pre-deployment enrollment token via
// the daemon and exposes the resulting ID through the flash bar. The
// operator runs `qu enroll list` (or watches the cluster.yaml diff)
// to retrieve the full base64 token — it is too long to display
// comfortably in the TUI's single-line flash, so we just confirm
// creation and point them at the CLI for the token string itself.
func newAddNodeForm() *form {
	fields := []formField{
		textField("Name", "optional label for this token (e.g. host name)", false),
		textField("TTL", "how long the token is valid (default 1h)", false),
		textField("Auto-approve", "y to skip cluster-side approval, blank for manual", false),
	}
	return newForm("Add peer (enrollment token)", fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			name := strings.TrimSpace(vals[0])
			ttl := 1 * time.Hour
			if raw := strings.TrimSpace(vals[1]); raw != "" {
				parsed, err := time.ParseDuration(raw)
				if err != nil {
					return formSubmitErr(fmt.Sprintf("ttl: %v", err))
				}
				ttl = parsed
			}
			auto := false
			switch strings.ToLower(strings.TrimSpace(vals[2])) {
			case "", "n", "no", "false":
				auto = false
			case "y", "yes", "true":
				auto = true
			default:
				return formSubmitErr("auto-approve must be y/n")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			raw, err := callDaemon(ctx, daemon.CtrlEnrollCreate, daemon.EnrollCreateBody{
				Name:        name,
				TTL:         ttl,
				AutoApprove: auto,
			})
			if err != nil {
				return formSubmitErr(err.Error())
			}
			var res daemon.EnrollCreateResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return formSubmitErr(err.Error())
			}
			approval := "manual"
			if res.AutoApprove {
				approval = "auto"
			}
			// The token is the long base64 string the joiner runs. It
			// is only available at create time (cluster.yaml stores
			// just the hash), so we surface the full command in the
			// flash — wrapping is fine, the user copies the whole
			// line. They can always revoke with `qu enroll revoke
			// <id>` if they typoed something.
			return modalDone{
				flash: fmt.Sprintf("token %s created (%s, expires in %s). run on the new host:\n  qu enroll join %s",
					res.ID, approval, time.Until(res.ExpiresAt).Round(time.Second), res.Token),
				level: flashInfo,
			}
		}
	})
}

// =============================================================
// Edit forms — same shape as the add forms above, but the inputs are
// pre-populated from an existing record and the submit closure reuses
// the original ID so the daemon's upsert path replaces the entry
// instead of creating a new one.
// =============================================================

func newEditCheckForm(existing config.Check) *form {
	intervalStr := ""
	if existing.Interval > 0 {
		intervalStr = existing.Interval.String()
	}
	timeoutStr := ""
	if existing.Timeout > 0 {
		timeoutStr = existing.Timeout.String()
	}
	expectStr := ""
	if existing.ExpectStatus > 0 {
		expectStr = fmt.Sprintf("%d", existing.ExpectStatus)
	}
	warnDaysStr := ""
	if existing.TLSWarnDays > 0 {
		warnDaysStr = fmt.Sprintf("%d", existing.TLSWarnDays)
	}

	fields := []formField{
		textFieldWithValue("Name", "human-friendly identifier", existing.Name, true),
		textFieldWithValue("Target", targetHint(existing.Type), existing.Target, true),
		textFieldWithValue("Interval", "e.g. 30s, 1m", intervalStr, false),
		textFieldWithValue("Timeout", "e.g. 10s", timeoutStr, false),
		textFieldWithValue("Alerts", "comma-separated alert IDs/names (optional)", strings.Join(existing.AlertIDs, ","), false),
	}
	switch existing.Type {
	case config.CheckHTTP:
		fields = append(fields,
			textFieldWithValue("Expect status", "e.g. 200 (HTTP only)", expectStr, false),
			textFieldWithValue("Body match", "substring required (HTTP only)", existing.BodyMatch, false),
		)
	case config.CheckTLS:
		fields = append(fields,
			textFieldWithValue("Warn days", "fail when cert expires within N days (default 14)", warnDaysStr, false),
			textFieldWithValue("SNI", "override server name; blank = host from target", existing.TLSServerName, false),
		)
	case config.CheckDNS:
		fields = append(fields,
			textFieldWithValue("Record", "a | aaaa | cname | mx | txt | ns (default a)", existing.DNSRecord, false),
			textFieldWithValue("Resolver", "host:port; blank = system resolver", existing.DNSResolver, false),
			textFieldWithValue("Expect", "substring required in an answer (optional)", existing.DNSExpect, false),
		)
	}
	checkType := existing.Type
	id := existing.ID
	suppress := append([]string(nil), existing.SuppressAlertIDs...)
	return newForm("Edit "+strings.ToUpper(string(checkType))+" check", fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			ch := config.Check{
				ID:               id,
				Name:             strings.TrimSpace(vals[0]),
				Type:             checkType,
				Target:           strings.TrimSpace(vals[1]),
				Interval:         parseDurationOr(vals[2], 30*time.Second),
				Timeout:          parseDurationOr(vals[3], 10*time.Second),
				SuppressAlertIDs: suppress,
			}
			if a := strings.TrimSpace(vals[4]); a != "" {
				for _, p := range strings.Split(a, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						ch.AlertIDs = append(ch.AlertIDs, p)
					}
				}
			}
			switch checkType {
			case config.CheckHTTP:
				ch.ExpectStatus = atoiOr(vals[5], 200)
				ch.BodyMatch = strings.TrimSpace(vals[6])
			case config.CheckTLS:
				ch.TLSWarnDays = atoiOr(vals[5], 14)
				ch.TLSServerName = strings.TrimSpace(vals[6])
			case config.CheckDNS:
				ch.DNSRecord = strings.ToLower(strings.TrimSpace(vals[5]))
				ch.DNSResolver = strings.TrimSpace(vals[6])
				ch.DNSExpect = strings.TrimSpace(vals[7])
			}
			if err := mutateAdd(transport.MutationAddCheck, ch); err != nil {
				return formSubmitErr(err.Error())
			}
			return modalDone{flash: "updated check " + ch.Name, level: flashInfo}
		}
	})
}

func newEditDiscordForm(existing config.Alert) *form {
	fields := []formField{
		textFieldWithValue("Name", "human-friendly identifier", existing.Name, true),
		textFieldWithValue("Webhook URL", "https://discord.com/api/webhooks/...", existing.DiscordWebhook, true),
		textFieldWithValue("Default", "yes/no — attach to every check automatically", boolStr(existing.Default), false),
		textAreaFieldWithValue("Body template", alerts.TemplateVarsHint(), existing.BodyTemplate, false),
	}
	id := existing.ID
	subject := existing.SubjectTemplate
	return newForm("Edit Discord alert", fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			a := config.Alert{
				ID:              id,
				Name:            strings.TrimSpace(vals[0]),
				Type:            config.AlertDiscord,
				DiscordWebhook:  strings.TrimSpace(vals[1]),
				Default:         parseBool(vals[2]),
				BodyTemplate:    vals[3],
				SubjectTemplate: subject,
			}
			if err := mutateAdd(transport.MutationAddAlert, a); err != nil {
				return formSubmitErr(err.Error())
			}
			return modalDone{flash: "updated discord alert " + a.Name, level: flashInfo}
		}
	})
}

func newEditSMTPForm(existing config.Alert) *form {
	portStr := ""
	if existing.SMTPPort > 0 {
		portStr = fmt.Sprintf("%d", existing.SMTPPort)
	}
	fields := []formField{
		textFieldWithValue("Name", "human-friendly identifier", existing.Name, true),
		textFieldWithValue("Host", "smtp.example.com", existing.SMTPHost, true),
		textFieldWithValue("Port", "default 587", portStr, false),
		textFieldWithValue("User", "leave empty for anonymous", existing.SMTPUser, false),
		passwordFieldWithValue("Password", "smtp auth password", existing.SMTPPassword),
		textFieldWithValue("From", "envelope From address", existing.SMTPFrom, true),
		textFieldWithValue("To", "comma-separated recipient addresses", strings.Join(existing.SMTPTo, ","), true),
		textFieldWithValue("StartTLS", "yes/no — default yes", boolStr(existing.SMTPStartTLS), false),
		textFieldWithValue("Default", "yes/no — attach to every check", boolStr(existing.Default), false),
		textFieldWithValue("Subject template", alerts.TemplateVarsHint(), existing.SubjectTemplate, false),
		textAreaFieldWithValue("Body template", alerts.TemplateVarsHint(), existing.BodyTemplate, false),
	}
	id := existing.ID
	return newForm("Edit SMTP alert", fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			to := strings.Split(strings.TrimSpace(vals[6]), ",")
			for i := range to {
				to[i] = strings.TrimSpace(to[i])
			}
			a := config.Alert{
				ID:              id,
				Name:            strings.TrimSpace(vals[0]),
				Type:            config.AlertSMTP,
				SMTPHost:        strings.TrimSpace(vals[1]),
				SMTPPort:        atoiOr(vals[2], 587),
				SMTPUser:        strings.TrimSpace(vals[3]),
				SMTPPassword:    vals[4],
				SMTPFrom:        strings.TrimSpace(vals[5]),
				SMTPTo:          to,
				SMTPStartTLS:    parseBoolOr(vals[7], true),
				Default:         parseBool(vals[8]),
				SubjectTemplate: vals[9],
				BodyTemplate:    vals[10],
			}
			if err := mutateAdd(transport.MutationAddAlert, a); err != nil {
				return formSubmitErr(err.Error())
			}
			return modalDone{flash: "updated smtp alert " + a.Name, level: flashInfo}
		}
	})
}

// newEditNodeForm only exposes the advertise address. The NodeID and
// fingerprint/cert are bound by trust and cannot be edited in place;
// removing and re-adding the node is the path for those changes.
func newEditNodeForm(existing config.PeerInfo) *form {
	fields := []formField{
		textFieldWithValue("Address", "host:9901 — peer's advertise endpoint", existing.Advertise, true),
	}
	id := existing.NodeID
	fp := existing.Fingerprint
	cert := existing.CertPEM
	return newForm("Edit node "+shortID(id), fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			p := config.PeerInfo{
				NodeID:      id,
				Advertise:   strings.TrimSpace(vals[0]),
				Fingerprint: fp,
				CertPEM:     cert,
			}
			if err := mutateAdd(transport.MutationAddPeer, p); err != nil {
				return formSubmitErr(err.Error())
			}
			return modalDone{flash: "updated node " + shortID(id), level: flashInfo}
		}
	})
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// =============================================================
// Pickers and confirmations.
// =============================================================

type pickerOption struct {
	label  string
	hint   string
	choose func() modal
	act    func() tea.Cmd // if non-nil, picker returns this cmd directly instead of opening another modal
}

type picker struct {
	title   string
	options []pickerOption
	cursor  int
}

func newPicker(title string, options []pickerOption) *picker {
	return &picker{title: title, options: options}
}

func (p *picker) Title() string { return p.title }

func (p *picker) Init() tea.Cmd { return nil }

func (p *picker) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", titleStyle.Render(p.title))
	for i, o := range p.options {
		marker := "  "
		style := subtleStyle
		if i == p.cursor {
			marker = "▸ "
			style = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
		}
		fmt.Fprintf(&b, "%s%s\n", marker, style.Render(o.label))
		if o.hint != "" {
			fmt.Fprintf(&b, "    %s\n", helpStyle.Render(o.hint))
		}
	}
	fmt.Fprintf(&b, "\n%s\n", helpStyle.Render("↑↓ select   enter pick   esc cancel"))
	return b.String()
}

func (p *picker) Update(msg tea.Msg) (modal, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	switch km.String() {
	case "esc":
		return p, modalDoneCmd("", flashInfo)
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
		return p, nil
	case "down", "j":
		if p.cursor < len(p.options)-1 {
			p.cursor++
		}
		return p, nil
	case "enter":
		if p.cursor < 0 || p.cursor >= len(p.options) {
			return p, nil
		}
		opt := p.options[p.cursor]
		if opt.act != nil {
			return p, opt.act()
		}
		if opt.choose != nil {
			return opt.choose(), nil
		}
	}
	return p, nil
}

// confirm asks yes/no and runs onConfirm if the user picks yes.
type confirm struct {
	prompt    string
	onConfirm func() tea.Cmd
	choice    int // 0=no, 1=yes
	busy      bool
	err       string
}

func newConfirm(prompt string, onConfirm func() tea.Cmd) *confirm {
	return &confirm{prompt: prompt, onConfirm: onConfirm}
}

func (c *confirm) Title() string { return "Confirm" }

func (c *confirm) Init() tea.Cmd { return nil }

func (c *confirm) View() string {
	noStyle, yesStyle := subtleStyle, subtleStyle
	if c.choice == 0 {
		noStyle = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	} else {
		yesStyle = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	}
	body := fmt.Sprintf("%s\n\n  [%s]   [%s]\n",
		c.prompt,
		noStyle.Render("No"),
		yesStyle.Render("Yes"),
	)
	if c.err != "" {
		body += "\n" + flashErrorStyle.Render("error: "+c.err) + "\n"
	}
	if c.busy {
		body += "\n" + flashWarnStyle.Render("working…") + "\n"
	} else {
		body += "\n" + helpStyle.Render("←→ or h/l select   enter confirm   esc cancel")
	}
	return body
}

func (c *confirm) Update(msg tea.Msg) (modal, tea.Cmd) {
	switch msg := msg.(type) {
	case formSubmitErr:
		c.busy = false
		c.err = string(msg)
		return c, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return c, modalDoneCmd("", flashInfo)
		case "left", "right", "h", "l", "tab":
			c.choice = 1 - c.choice
			return c, nil
		case "y", "Y":
			c.choice = 1
			return c.commit()
		case "n", "N":
			return c, modalDoneCmd("cancelled", flashInfo)
		case "enter":
			if c.choice == 1 {
				return c.commit()
			}
			return c, modalDoneCmd("cancelled", flashInfo)
		}
	}
	return c, nil
}

func (c *confirm) commit() (modal, tea.Cmd) {
	if c.busy {
		return c, nil
	}
	c.busy = true
	c.err = ""
	return c, c.onConfirm()
}

// =============================================================
// Helpers shared by submit closures.
// =============================================================

func mutateAdd(kind transport.MutationKind, payload any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	body := daemon.MutateBody{Kind: kind, Payload: raw}
	_, err = callDaemon(ctx, daemon.CtrlMutate, body)
	return err
}

func mutateRemove(kind transport.MutationKind, idOrName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := json.Marshal(idOrName)
	if err != nil {
		return err
	}
	body := daemon.MutateBody{Kind: kind, Payload: raw}
	_, err = callDaemon(ctx, daemon.CtrlMutate, body)
	return err
}

func targetHint(t config.CheckType) string {
	switch t {
	case config.CheckHTTP:
		return "https://example.com/health"
	case config.CheckTCP:
		return "db.internal:5432"
	case config.CheckICMP:
		return "10.0.0.1"
	case config.CheckTLS:
		return "example.com[:443] or https://example.com"
	case config.CheckDNS:
		return "example.com"
	}
	return ""
}

func parseDurationOr(s string, fallback time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

func atoiOr(s string, fallback int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return fallback
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "y", "true", "t", "on", "1":
		return true
	}
	return false
}

func parseBoolOr(s string, fallback bool) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return fallback
	}
	switch s {
	case "yes", "y", "true", "t", "on", "1":
		return true
	case "no", "n", "false", "f", "off", "0":
		return false
	}
	return fallback
}
