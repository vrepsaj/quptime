// Package tui implements the interactive overview/control surface
// reachable via `qu tui`. It is a thin bubbletea client over the same
// unix control socket the CLI uses; nothing here talks to peers
// directly.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

const refreshInterval = 2 * time.Second

// Run starts the bubbletea program. Blocks until the user quits.
func Run() error {
	m := initialModel()
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}

type tabIndex int

const (
	tabPeers tabIndex = iota
	tabChecks
	tabAlerts
)

var tabNames = []string{"Peers", "Checks", "Alerts"}

type model struct {
	width, height int

	status       transport.StatusResponse
	statusLoaded bool
	statusErr    string

	// Full records cached from cluster.yaml directly (the daemon status
	// only ships per-check effective alert names and per-peer liveness).
	// We need the full records to render the alerts tab, to support the
	// default-toggle, and to pre-fill edit forms with current values.
	peersFull  []config.PeerInfo
	checksFull []config.Check
	alerts     []config.Alert

	active tabIndex
	peers  *peersTab
	checks *checksTab
	alertsT *alertsTab

	modal modal

	flash      string
	flashLevel flashLevel
	flashUntil time.Time
}

func initialModel() model {
	return model{
		peers:   newPeersTab(),
		checks:  newChecksTab(),
		alertsT: newAlertsTab(),
	}
}

// =============================================================
// Bubbletea lifecycle.
// =============================================================

func (m model) Init() tea.Cmd {
	return tea.Batch(loadStatusCmd(), loadConfigCmd(), tickCmd())
}

type tickMsg time.Time

type statusMsg struct {
	st  transport.StatusResponse
	err error
}

type configMsg struct {
	peers  []config.PeerInfo
	checks []config.Check
	alerts []config.Alert
	err    error
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func loadStatusCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		raw, err := callDaemon(ctx, daemon.CtrlStatus, nil)
		if err != nil {
			return statusMsg{err: err}
		}
		var st transport.StatusResponse
		if err := json.Unmarshal(raw, &st); err != nil {
			return statusMsg{err: err}
		}
		return statusMsg{st: st}
	}
}

func loadConfigCmd() tea.Cmd {
	return func() tea.Msg {
		cfg, err := config.LoadClusterConfig()
		if err != nil {
			return configMsg{err: err}
		}
		snap := cfg.Snapshot()
		return configMsg{peers: snap.Peers, checks: snap.Checks, alerts: snap.Alerts}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeTabs()
		if m.modal != nil {
			m.modal, _ = m.modal.Update(msg)
		}
		return m, nil

	case tickMsg:
		return m, tea.Batch(loadStatusCmd(), loadConfigCmd(), tickCmd())

	case statusMsg:
		if msg.err != nil {
			m.statusErr = msg.err.Error()
		} else {
			m.statusErr = ""
			wasLoaded := m.statusLoaded
			m.status = msg.st
			m.statusLoaded = true
			m.peers.Refresh(msg.st, msg.st.NodeID)
			m.checks.Refresh(msg.st)
			// First load may change header height on narrow terminals;
			// re-run the layout so the body shrinks to compensate.
			if !wasLoaded {
				m.resizeTabs()
			}
		}
		return m, nil

	case configMsg:
		if msg.err == nil {
			m.peersFull = msg.peers
			m.checksFull = msg.checks
			m.alerts = msg.alerts
			m.alertsT.Refresh(toAlertRows(msg.alerts))
		}
		return m, nil

	case modalDone:
		m.modal = nil
		if msg.flash != "" {
			m.setFlash(msg.flash, msg.level)
		}
		// Force-refresh in case the modal mutated cluster state.
		return m, tea.Batch(loadStatusCmd(), loadConfigCmd())
	}

	// Modal grabs all input while open.
	if m.modal != nil {
		prev := m.modal
		newModal, cmd := m.modal.Update(msg)
		m.modal = newModal
		// If the modal handed off to a different modal (e.g. picker →
		// form), seed the new one with the current terminal size so
		// its text inputs can size themselves on first paint, and
		// dispatch its Init cmd so v2's cursor blink starts immediately.
		if newModal != nil && newModal != prev {
			cmd = tea.Batch(cmd, m.installModal())
		}
		return m, cmd
	}

	if km, ok := msg.(tea.KeyMsg); ok {
		return m.handleKey(km)
	}

	// Pass through to the active tab so j/k/PgUp/PgDn scroll the table.
	return m, m.forwardToActiveTab(msg)
}

func (m model) handleKey(km tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch km.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "tab", "right", "L":
		m.active = (m.active + 1) % 3
		return m, tea.ClearScreen
	case "shift+tab", "left", "H":
		m.active = (m.active + 2) % 3
		return m, tea.ClearScreen
	case "1", "2", "3":
		m.active = tabIndex(km.String()[0] - '1')
		return m, tea.ClearScreen
	case "r":
		m.setFlash("refreshing…", flashInfo)
		return m, tea.Batch(loadStatusCmd(), loadConfigCmd())
	case "a":
		m.modal = m.openAddPicker()
		return m, m.installModal()
	case "d":
		return m.openRemoveConfirm()
	case "e":
		return m.openEditForm()
	case "t":
		if m.active == tabAlerts {
			return m.testSelectedAlert()
		}
		if m.active == tabChecks {
			return m.openTestCheckPicker()
		}
	case "D":
		if m.active == tabAlerts {
			return m.toggleSelectedDefault()
		}
	case "x":
		switch m.active {
		case tabChecks:
			return m.toggleSelectedCheckEnabled()
		case tabAlerts:
			return m.toggleSelectedAlertEnabled()
		}
	}

	// Forward everything else (arrow keys etc.) to the active tab.
	return m, m.forwardToActiveTab(km)
}

// forwardToActiveTab passes msg to whichever tab is currently focused.
// Used for both arbitrary tea.Msg pass-through (mouse, ticks) and for
// keys handleKey didn't claim.
func (m model) forwardToActiveTab(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch m.active {
	case tabPeers:
		_, cmd = m.peers.Update(msg)
	case tabChecks:
		_, cmd = m.checks.Update(msg)
	case tabAlerts:
		_, cmd = m.alertsT.Update(msg)
	}
	return cmd
}

// =============================================================
// View.
// =============================================================

func (m model) View() tea.View {
	view := tea.View{AltScreen: true}
	if m.width == 0 {
		view.Content = "loading…"
		return view
	}
	header := m.renderHeader()
	tabs := m.renderTabs()
	body := m.renderActiveTab()
	help := m.renderHelp()

	page := lipgloss.JoinVertical(lipgloss.Left, header, tabs, body, m.renderFlash(), help)

	if m.modal != nil {
		overlay := modalStyle.Render(m.modal.View())
		view.Content = lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, overlay, lipgloss.WithWhitespaceChars(" "))
		return view
	}
	view.Content = page
	return view
}

func (m model) renderHeader() string {
	// lipgloss v2 includes the border in the value passed to Width (v1
	// did not). headerStyle has Border + Padding(0,1), so the usable
	// content area is outerW - 4 (2 border cols + 2 padding cols). We
	// want the box to fill the full terminal width, so outerW = m.width.
	outerW := m.width
	if outerW < 20 {
		outerW = 20
	}
	innerW := outerW - 4
	if innerW < 1 {
		innerW = 1
	}

	if !m.statusLoaded {
		msg := "connecting to daemon…"
		if m.statusErr != "" {
			msg = "daemon: " + m.statusErr
		}
		return headerStyle.Width(outerW).Render(titleStyle.Render("QUptime") + "  " + helpStyle.Render(msg))
	}
	st := m.status
	quorum := stateDownStyle.Render("● no quorum")
	if st.HasQuorum {
		quorum = stateUpStyle.Render(fmt.Sprintf("● quorum %d/%d", liveCount(st.Peers), st.QuorumSize))
	}
	master := stateUnknownStyle.Render("master: —")
	if st.MasterID != "" {
		master = "master: " + shortID(st.MasterID)
	}
	role := ""
	if st.NodeID == st.MasterID {
		role = stateUpStyle.Render("(you are master)")
	} else {
		role = subtleStyle.Render("(follower)")
	}
	left := lipgloss.JoinHorizontal(lipgloss.Top,
		titleStyle.Render("QUptime"),
		"  ",
		"node: "+shortID(st.NodeID),
		"  ",
		master,
		"  ",
		role,
	)
	right := lipgloss.JoinHorizontal(lipgloss.Top,
		quorum,
		"  ",
		subtleStyle.Render(fmt.Sprintf("term %d   ver %d", st.Term, st.Version)),
	)
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)

	// Single row when both halves fit with at least one space between them.
	if leftW+rightW+1 <= innerW {
		gap := innerW - leftW - rightW
		row := left + strings.Repeat(" ", gap) + right
		return headerStyle.Width(outerW).Render(row)
	}

	// Otherwise stack vertically so nothing gets clipped on narrow terminals.
	rows := lipgloss.JoinVertical(lipgloss.Left, left, right)
	return headerStyle.Width(outerW).Render(rows)
}

// headerHeight returns the actual number of terminal rows renderHeader
// produces, including the rounded border. Used to compute the body area in
// resizeTabs. We measure the rendered output rather than guess because the
// header's content can line-wrap on very narrow terminals (e.g. the left
// half being wider than the inner content area), which a width-based
// heuristic can't see.
func (m model) headerHeight() int {
	if m.width == 0 {
		return 3
	}
	return lipgloss.Height(m.renderHeader())
}

func (m model) renderTabs() string {
	parts := make([]string, len(tabNames))
	for i, name := range tabNames {
		count := ""
		switch tabIndex(i) {
		case tabPeers:
			count = fmt.Sprintf(" (%d)", len(m.status.Peers))
		case tabChecks:
			count = fmt.Sprintf(" (%d)", len(m.status.Checks))
		case tabAlerts:
			count = fmt.Sprintf(" (%d)", len(m.alerts))
		}
		label := name + count
		if tabIndex(i) == m.active {
			parts[i] = tabActiveStyle.Render(label)
		} else {
			parts[i] = tabIdleStyle.Render(fmt.Sprintf("[%d] %s", i+1, label))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

func (m model) renderActiveTab() string {
	var view string
	switch m.active {
	case tabPeers:
		view = m.peers.View()
	case tabChecks:
		view = m.checks.View()
	case tabAlerts:
		view = m.alertsT.View()
	}
	// Table columns can sum to more than the terminal width on narrow
	// terminals. Without this, bodyStyle.Width(...) would wrap each over-wide
	// row onto extra lines and push the page taller than m.height, clipping
	// the top of the TUI. Truncate per line so the bordered box stays the
	// exact bodyH rows we sized for.
	// bodyStyle has Border + Padding(0,1), and lipgloss v2 includes the
	// border in Width. So Width(m.width) gives a content area of
	// m.width - 4, which matches the MaxWidth clip below.
	innerW := m.width - 4
	if innerW < 1 {
		innerW = 1
	}
	view = lipgloss.NewStyle().MaxWidth(innerW).Render(view)
	return bodyStyle.Width(m.width).Render(view)
}

func (m model) renderHelp() string {
	specific := ""
	switch m.active {
	case tabPeers:
		specific = "a add  e edit  d remove"
	case tabChecks:
		specific = "a add  e edit  d remove  t test  x toggle on/off"
	case tabAlerts:
		specific = "a add  e edit  d remove  t test  D toggle default  x toggle on/off"
	}
	return helpStyle.Render(fmt.Sprintf("↑↓ navigate   ⇥ next tab   1/2/3 jump   r refresh   %s   q quit", specific))
}

func (m model) renderFlash() string {
	if m.flash == "" || time.Now().After(m.flashUntil) {
		return ""
	}
	switch m.flashLevel {
	case flashError:
		return flashErrorStyle.Render(m.flash)
	case flashWarn:
		return flashWarnStyle.Render(m.flash)
	default:
		return flashInfoStyle.Render(m.flash)
	}
}

// =============================================================
// Actions.
// =============================================================

func (m model) openAddPicker() modal {
	switch m.active {
	case tabPeers:
		return newAddNodeForm()
	case tabChecks:
		return newPicker("Add check — pick type", []pickerOption{
			{label: "HTTP", hint: "url + status code", choose: func() modal { return newAddCheckForm(config.CheckHTTP) }},
			{label: "TCP", hint: "host:port connect", choose: func() modal { return newAddCheckForm(config.CheckTCP) }},
			{label: "ICMP", hint: "ping a host", choose: func() modal { return newAddCheckForm(config.CheckICMP) }},
			{label: "TLS", hint: "cert expiry warning", choose: func() modal { return newAddCheckForm(config.CheckTLS) }},
			{label: "DNS", hint: "record resolution", choose: func() modal { return newAddCheckForm(config.CheckDNS) }},
		})
	case tabAlerts:
		return newPicker("Add alert — pick type", []pickerOption{
			{label: "Discord", hint: "webhook URL", choose: func() modal { return newAddDiscordForm() }},
			{label: "SMTP", hint: "email via relay", choose: func() modal { return newAddSMTPForm() }},
		})
	}
	return nil
}

func (m model) openRemoveConfirm() (tea.Model, tea.Cmd) {
	var prompt string
	var run func() tea.Cmd
	switch m.active {
	case tabPeers:
		id := m.peers.Selected()
		name := strings.TrimPrefix(m.peers.SelectedName(), "* ")
		if id == "" {
			return m, nil
		}
		id = strings.TrimPrefix(id, "* ")
		prompt = fmt.Sprintf("Remove peer %s from the cluster?\nThis revokes trust and updates cluster.yaml.", shortID(name))
		run = func() tea.Cmd {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if _, err := callDaemon(ctx, daemon.CtrlNodeRemove, daemon.NodeRemoveBody{NodeID: id}); err != nil {
					return formSubmitErr(err.Error())
				}
				return modalDone{flash: "removed node " + shortID(id), level: flashInfo}
			}
		}
	case tabChecks:
		id := m.checks.Selected()
		name := m.checks.SelectedName()
		if id == "" {
			return m, nil
		}
		prompt = fmt.Sprintf("Remove check %q?", name)
		run = func() tea.Cmd {
			return func() tea.Msg {
				if err := mutateRemove(transport.MutationRemoveCheck, id); err != nil {
					return formSubmitErr(err.Error())
				}
				return modalDone{flash: "removed check " + name, level: flashInfo}
			}
		}
	case tabAlerts:
		id := m.alertsT.Selected()
		name := m.alertsT.SelectedName()
		if id == "" {
			return m, nil
		}
		prompt = fmt.Sprintf("Remove alert %q?", name)
		run = func() tea.Cmd {
			return func() tea.Msg {
				if err := mutateRemove(transport.MutationRemoveAlert, id); err != nil {
					return formSubmitErr(err.Error())
				}
				return modalDone{flash: "removed alert " + name, level: flashInfo}
			}
		}
	default:
		return m, nil
	}
	m.modal = newConfirm(prompt, run)
	return m, m.installModal()
}

// installModal feeds the current terminal size to the modal so its
// inputs can size themselves on first paint, and returns the modal's
// Init cmd. v2 forms produce a blink Cmd from Init that drives the
// cursor animation — dispatch it so the cursor starts blinking the
// moment the modal appears.
func (m *model) installModal() tea.Cmd {
	if m.modal == nil {
		return nil
	}
	if m.width > 0 {
		m.modal, _ = m.modal.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	}
	return m.modal.Init()
}

// openEditForm dispatches to the right pre-filled edit form based on the
// active tab and the row under the cursor. Looks up the full record in
// m.peersFull / m.checksFull / m.alerts (populated by loadConfigCmd) so
// the form starts with the entry's current values rather than blanks.
func (m model) openEditForm() (tea.Model, tea.Cmd) {
	switch m.active {
	case tabPeers:
		id := strings.TrimPrefix(m.peers.Selected(), "* ")
		if id == "" {
			m.setFlash("no peer selected", flashWarn)
			return m, nil
		}
		for i := range m.peersFull {
			if m.peersFull[i].NodeID == id {
				m.modal = newEditNodeForm(m.peersFull[i])
				return m, m.installModal()
			}
		}
		m.setFlash("peer not found in local cluster.yaml", flashError)
		return m, nil

	case tabChecks:
		id := m.checks.Selected()
		if id == "" {
			m.setFlash("no check selected", flashWarn)
			return m, nil
		}
		for i := range m.checksFull {
			if m.checksFull[i].ID == id {
				m.modal = newEditCheckForm(m.checksFull[i])
				return m, m.installModal()
			}
		}
		m.setFlash("check not found in local cluster.yaml", flashError)
		return m, nil

	case tabAlerts:
		id := m.alertsT.Selected()
		if id == "" {
			m.setFlash("no alert selected", flashWarn)
			return m, nil
		}
		for i := range m.alerts {
			if m.alerts[i].ID != id {
				continue
			}
			switch m.alerts[i].Type {
			case config.AlertDiscord:
				m.modal = newEditDiscordForm(m.alerts[i])
			case config.AlertSMTP:
				m.modal = newEditSMTPForm(m.alerts[i])
			default:
				m.setFlash("unsupported alert type", flashError)
				return m, nil
			}
			return m, m.installModal()
		}
		m.setFlash("alert not found in local cluster.yaml", flashError)
		return m, nil
	}
	return m, nil
}

// openTestCheckPicker pops a small picker over the Checks tab asking
// which synthetic transition to fire (down / up / recovered), then
// ships the choice to the daemon. The picker is dismissed before the
// daemon call so the user sees the flash and not a stuck modal.
func (m model) openTestCheckPicker() (tea.Model, tea.Cmd) {
	id := m.checks.Selected()
	if id == "" {
		return m, nil
	}
	name := m.checks.SelectedName()
	fire := func(state, verb string) func() tea.Cmd {
		return func() tea.Cmd {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				body := daemon.CheckTestBody{CheckID: id, State: state}
				if _, err := callDaemon(ctx, daemon.CtrlCheckTest, body); err != nil {
					return modalDone{flash: "test failed: " + err.Error(), level: flashError}
				}
				return modalDone{flash: "fired synthetic " + verb + " for " + name, level: flashInfo}
			}
		}
	}
	m.modal = newPicker("Test alert for "+name+" — pick transition", []pickerOption{
		{label: "DOWN", hint: "Up → Down (most common test)", act: fire("down", "DOWN")},
		{label: "RECOVERED", hint: "Down → Up — exercise the recovery message", act: fire("recovered", "RECOVERED")},
		{label: "UP", hint: "Unknown → Up — bypasses the normal cold-start suppression", act: fire("up", "UP")},
	})
	return m, m.installModal()
}

func (m model) testSelectedAlert() (tea.Model, tea.Cmd) {
	id := m.alertsT.Selected()
	if id == "" {
		return m, nil
	}
	name := m.alertsT.SelectedName()
	m.setFlash("sending test to "+name+"…", flashInfo)
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := callDaemon(ctx, daemon.CtrlAlertTest, daemon.AlertTestBody{AlertID: id}); err != nil {
			return modalDone{flash: "test failed: " + err.Error(), level: flashError}
		}
		return modalDone{flash: "test sent via " + name, level: flashInfo}
	}
}

func (m model) toggleSelectedCheckEnabled() (tea.Model, tea.Cmd) {
	id := m.checks.Selected()
	if id == "" {
		return m, nil
	}
	var target *config.Check
	for i := range m.checksFull {
		if m.checksFull[i].ID == id {
			cp := m.checksFull[i]
			target = &cp
			break
		}
	}
	if target == nil {
		m.setFlash("check not found in local cluster.yaml", flashError)
		return m, nil
	}
	target.Disabled = !target.Disabled
	name := target.Name
	verb := "enabled"
	if target.Disabled {
		verb = "disabled"
	}
	return m, func() tea.Msg {
		if err := mutateAdd(transport.MutationAddCheck, target); err != nil {
			return modalDone{flash: "toggle failed: " + err.Error(), level: flashError}
		}
		return modalDone{flash: fmt.Sprintf("check %s %s", name, verb), level: flashInfo}
	}
}

func (m model) toggleSelectedAlertEnabled() (tea.Model, tea.Cmd) {
	id := m.alertsT.Selected()
	if id == "" {
		return m, nil
	}
	var target *config.Alert
	for i := range m.alerts {
		if m.alerts[i].ID == id {
			cp := m.alerts[i]
			target = &cp
			break
		}
	}
	if target == nil {
		m.setFlash("alert not found in local cluster.yaml", flashError)
		return m, nil
	}
	target.Disabled = !target.Disabled
	name := target.Name
	verb := "enabled"
	if target.Disabled {
		verb = "disabled"
	}
	return m, func() tea.Msg {
		if err := mutateAdd(transport.MutationAddAlert, target); err != nil {
			return modalDone{flash: "toggle failed: " + err.Error(), level: flashError}
		}
		return modalDone{flash: fmt.Sprintf("alert %s %s", name, verb), level: flashInfo}
	}
}

func (m model) toggleSelectedDefault() (tea.Model, tea.Cmd) {
	row := m.alertsT.SelectedAlert()
	if row == nil {
		return m, nil
	}
	var target *config.Alert
	for i := range m.alerts {
		if m.alerts[i].ID == row.ID {
			cp := m.alerts[i]
			target = &cp
			break
		}
	}
	if target == nil {
		m.setFlash("alert not found in local cluster.yaml", flashError)
		return m, nil
	}
	target.Default = !target.Default
	name := target.Name
	newState := target.Default
	return m, func() tea.Msg {
		if err := mutateAdd(transport.MutationAddAlert, target); err != nil {
			return modalDone{flash: "toggle failed: " + err.Error(), level: flashError}
		}
		state := "off"
		if newState {
			state = "on"
		}
		return modalDone{flash: fmt.Sprintf("alert %s default=%s", name, state), level: flashInfo}
	}
}

// =============================================================
// Small helpers.
// =============================================================

func (m *model) setFlash(s string, level flashLevel) {
	m.flash = s
	m.flashLevel = level
	m.flashUntil = time.Now().Add(4 * time.Second)
}

func (m *model) resizeTabs() {
	// Rows consumed outside the body: header (variable), tabs (1),
	// body's own rounded border (2), flash (1), help (1). On terminals
	// too small to honor the reservation, shrink the body all the way
	// down to 1 row rather than letting the page overflow — the table
	// will collapse to a single visible row but the rest of the chrome
	// stays on screen.
	reserved := m.headerHeight() + 5
	bodyH := m.height - reserved
	if bodyH < 1 {
		bodyH = 1
	}
	bodyW := m.width - 4
	if bodyW < 20 {
		bodyW = 20
	}
	m.peers.SetSize(bodyW, bodyH)
	m.checks.SetSize(bodyW, bodyH)
	m.alertsT.SetSize(bodyW, bodyH)
}

func toAlertRows(alerts []config.Alert) []alertRow {
	out := make([]alertRow, 0, len(alerts))
	for _, a := range alerts {
		endpoint := ""
		switch a.Type {
		case config.AlertDiscord:
			endpoint = a.DiscordWebhook
		case config.AlertSMTP:
			endpoint = fmt.Sprintf("%s:%d → %s", a.SMTPHost, a.SMTPPort, strings.Join(a.SMTPTo, ","))
		}
		out = append(out, alertRow{
			ID:       a.ID,
			Name:     a.Name,
			Type:     string(a.Type),
			Enabled:  !a.Disabled,
			Default:  a.Default,
			HasTmpl:  a.SubjectTemplate != "" || a.BodyTemplate != "",
			Endpoint: endpoint,
		})
	}
	return out
}

func liveCount(peers []transport.PeerLiveness) int {
	n := 0
	for _, p := range peers {
		if p.Live {
			n++
		}
	}
	return n
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

