package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"

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
type modal interface {
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
	label    string
	input    textinput.Model
	required bool
	hint     string
}

type form struct {
	title  string
	fields []formField
	cursor int
	busy   bool
	err    string

	submit func(values []string) tea.Cmd
}

func newForm(title string, fields []formField, submit func([]string) tea.Cmd) *form {
	for i := range fields {
		fields[i].input.Prompt = ""
		fields[i].input.CharLimit = 256
		if i == 0 {
			fields[i].input.Focus()
		} else {
			fields[i].input.Blur()
		}
	}
	return &form{title: title, fields: fields, submit: submit}
}

func textField(label, hint string, required bool) formField {
	ti := textinput.New()
	ti.Width = 40
	ti.Placeholder = hint
	return formField{label: label, hint: hint, required: required, input: ti}
}

func passwordField(label, hint string) formField {
	ti := textinput.New()
	ti.Width = 40
	ti.Placeholder = hint
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	return formField{label: label, hint: hint, input: ti}
}

func (f *form) Title() string { return f.title }

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
		fmt.Fprintf(&b, "  %s\n", fld.input.View())
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
		fmt.Fprintf(&b, "%s\n", helpStyle.Render("↑↓ field   enter next/submit   esc cancel"))
	}
	return b.String()
}

func (f *form) Update(msg tea.Msg) (modal, tea.Cmd) {
	switch msg := msg.(type) {
	case formSubmitErr:
		f.busy = false
		f.err = string(msg)
		return f, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return f, modalDoneCmd("", flashInfo)
		case "tab", "down":
			f.advance(1)
			return f, nil
		case "shift+tab", "up":
			f.advance(-1)
			return f, nil
		case "enter":
			if f.busy {
				return f, nil
			}
			if f.cursor < len(f.fields)-1 {
				f.advance(1)
				return f, nil
			}
			vals := make([]string, len(f.fields))
			for i, fld := range f.fields {
				vals[i] = fld.input.Value()
			}
			for i, fld := range f.fields {
				if fld.required && strings.TrimSpace(vals[i]) == "" {
					f.err = fld.label + " is required"
					f.cursor = i
					f.focusOnly(i)
					return f, nil
				}
			}
			f.busy = true
			f.err = ""
			return f, f.submit(vals)
		}
	}
	var cmd tea.Cmd
	f.fields[f.cursor].input, cmd = f.fields[f.cursor].input.Update(msg)
	return f, cmd
}

func (f *form) advance(delta int) {
	n := len(f.fields)
	if n == 0 {
		return
	}
	f.cursor = (f.cursor + delta + n) % n
	f.focusOnly(f.cursor)
}

func (f *form) focusOnly(i int) {
	for j := range f.fields {
		if j == i {
			f.fields[j].input.Focus()
		} else {
			f.fields[j].input.Blur()
		}
	}
}

// formSubmitErr is a tea.Msg the submit cmd returns to surface an
// error inline without closing the form.
type formSubmitErr string

func submitErr(err error) tea.Cmd {
	return func() tea.Msg { return formSubmitErr(err.Error()) }
}

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
	if checkType == config.CheckHTTP {
		fields = append(fields,
			textField("Expect status", "e.g. 200 (HTTP only)", false),
			textField("Body match", "substring required (HTTP only)", false),
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
			if checkType == config.CheckHTTP {
				ch.ExpectStatus = atoiOr(vals[5], 200)
				ch.BodyMatch = strings.TrimSpace(vals[6])
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
		textField("Body template", "leave empty for default formatting", false),
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
		textField("Subject template", "optional", false),
		textField("Body template", "optional", false),
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

func newAddNodeForm() *form {
	fields := []formField{
		textField("Address", "host:9901 of the peer to invite", true),
	}
	return newForm("Add node (TOFU)", fields, func(vals []string) tea.Cmd {
		return func() tea.Msg {
			addr := strings.TrimSpace(vals[0])
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			raw, err := callDaemon(ctx, daemon.CtrlNodeProbe, daemon.NodeProbeBody{Address: addr})
			if err != nil {
				return formSubmitErr(fmt.Sprintf("probe: %v", err))
			}
			var probe daemon.NodeProbeResult
			if err := json.Unmarshal(raw, &probe); err != nil {
				return formSubmitErr(err.Error())
			}
			// auto-accept the fingerprint we just observed. The cluster
			// secret check on the remote side already prevents random
			// hosts from being trusted.
			raw, err = callDaemon(ctx, daemon.CtrlNodeAdd, daemon.NodeAddBody{
				Address:     addr,
				Fingerprint: probe.Fingerprint,
			})
			if err != nil {
				return formSubmitErr(fmt.Sprintf("add: %v", err))
			}
			var res daemon.NodeAddResult
			_ = json.Unmarshal(raw, &res)
			return modalDone{
				flash: fmt.Sprintf("added node %s — cluster version %d", res.NodeID, res.Version),
				level: flashInfo,
			}
		}
	})
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
