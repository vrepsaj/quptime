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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/daemon"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

const refreshInterval = 2 * time.Second

// Run starts the bubbletea program. Blocks until the user quits.
func Run() error {
	m := initialModel()
	p := tea.NewProgram(m, tea.WithAltScreen())
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

	// Cached alerts come from cluster.yaml directly (the daemon status
	// only ships per-check effective alert names). We need full Alert
	// records to render the alerts tab and to support default-toggle.
	alerts []config.Alert

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
	return tea.Batch(loadStatusCmd(), loadAlertsCmd(), tickCmd())
}

type tickMsg time.Time

type statusMsg struct {
	st  transport.StatusResponse
	err error
}

type alertsMsg struct {
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

func loadAlertsCmd() tea.Cmd {
	return func() tea.Msg {
		cfg, err := config.LoadClusterConfig()
		if err != nil {
			return alertsMsg{err: err}
		}
		snap := cfg.Snapshot()
		return alertsMsg{alerts: snap.Alerts}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeTabs()
		return m, nil

	case tickMsg:
		return m, tea.Batch(loadStatusCmd(), loadAlertsCmd(), tickCmd())

	case statusMsg:
		if msg.err != nil {
			m.statusErr = msg.err.Error()
		} else {
			m.statusErr = ""
			m.status = msg.st
			m.statusLoaded = true
			m.peers.Refresh(msg.st, msg.st.NodeID)
			m.checks.Refresh(msg.st)
		}
		return m, nil

	case alertsMsg:
		if msg.err == nil {
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
		return m, tea.Batch(loadStatusCmd(), loadAlertsCmd())
	}

	// Modal grabs all input while open.
	if m.modal != nil {
		newModal, cmd := m.modal.Update(msg)
		m.modal = newModal
		return m, cmd
	}

	if km, ok := msg.(tea.KeyMsg); ok {
		return m.handleKey(km)
	}

	// Pass through to the active tab so j/k/PgUp/PgDn scroll the table.
	switch m.active {
	case tabPeers:
		_, cmd := m.peers.Update(msg)
		return m, cmd
	case tabChecks:
		_, cmd := m.checks.Update(msg)
		return m, cmd
	case tabAlerts:
		_, cmd := m.alertsT.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) handleKey(km tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch km.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "tab", "right", "L":
		m.active = (m.active + 1) % 3
		return m, nil
	case "shift+tab", "left", "H":
		m.active = (m.active + 2) % 3
		return m, nil
	case "1":
		m.active = tabPeers
		return m, nil
	case "2":
		m.active = tabChecks
		return m, nil
	case "3":
		m.active = tabAlerts
		return m, nil
	case "r":
		m.setFlash("refreshing…", flashInfo)
		return m, tea.Batch(loadStatusCmd(), loadAlertsCmd())
	case "a":
		m.modal = m.openAddPicker()
		return m, nil
	case "d":
		return m.openRemoveConfirm()
	case "t":
		if m.active == tabAlerts {
			return m.testSelectedAlert()
		}
	case "D":
		if m.active == tabAlerts {
			return m.toggleSelectedDefault()
		}
	}

	// Forward everything else (arrow keys etc.) to the active tab.
	switch m.active {
	case tabPeers:
		_, cmd := m.peers.Update(km)
		return m, cmd
	case tabChecks:
		_, cmd := m.checks.Update(km)
		return m, cmd
	case tabAlerts:
		_, cmd := m.alertsT.Update(km)
		return m, cmd
	}
	return m, nil
}

// =============================================================
// View.
// =============================================================

func (m model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	header := m.renderHeader()
	tabs := m.renderTabs()
	body := m.renderActiveTab()
	help := m.renderHelp()

	page := lipgloss.JoinVertical(lipgloss.Left, header, tabs, body, m.renderFlash(), help)

	if m.modal != nil {
		overlay := modalStyle.Render(m.modal.View())
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, overlay, lipgloss.WithWhitespaceChars(" "))
	}
	return page
}

func (m model) renderHeader() string {
	if !m.statusLoaded {
		msg := "connecting to daemon…"
		if m.statusErr != "" {
			msg = "daemon: " + m.statusErr
		}
		return headerStyle.Width(m.width - 2).Render(titleStyle.Render("QUptime") + "  " + helpStyle.Render(msg))
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
	width := m.width - 2
	if width < 20 {
		width = 20
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	row := left + strings.Repeat(" ", gap) + right
	return headerStyle.Width(width).Render(row)
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
	return bodyStyle.Width(m.width - 2).Render(view)
}

func (m model) renderHelp() string {
	specific := ""
	switch m.active {
	case tabPeers:
		specific = "a add node   d remove node"
	case tabChecks:
		specific = "a add check  d remove check"
	case tabAlerts:
		specific = "a add alert  d remove alert  t test  D toggle default"
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
	return m, nil
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
	bodyH := m.height - 8
	if bodyH < 5 {
		bodyH = 5
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

