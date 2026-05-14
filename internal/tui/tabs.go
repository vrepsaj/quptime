package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"git.cer.sh/axodouble/quptime/internal/transport"
)

// tabModel is the small surface every tab implements. Tabs share the
// same Update/View shape so the parent can dispatch generically.
type tabModel interface {
	Update(tea.Msg) (tabModel, tea.Cmd)
	View() string
	SetSize(width, height int)
	// Selected returns the row identifier for the row under the cursor,
	// or "" if the table is empty. For peers/nodes this is a NodeID;
	// for checks it's a CheckID; for alerts it's an AlertID.
	Selected() string
	// SelectedName returns a human-friendly label for the selected row
	// (used in confirm dialogs).
	SelectedName() string
}

// peersTab — read-only view of cluster membership.
type peersTab struct {
	tbl table.Model
}

func newPeersTab() *peersTab {
	cols := []table.Column{
		{Title: "NODE_ID", Width: 38},
		{Title: "ADVERTISE", Width: 28},
		{Title: "LIVE", Width: 8},
		{Title: "LAST SEEN", Width: 22},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	t.SetStyles(tableStyles())
	return &peersTab{tbl: t}
}

func (p *peersTab) Update(msg tea.Msg) (tabModel, tea.Cmd) {
	var cmd tea.Cmd
	p.tbl, cmd = p.tbl.Update(msg)
	return p, cmd
}

func (p *peersTab) View() string { return p.tbl.View() }

func (p *peersTab) SetSize(w, h int) {
	p.tbl.SetWidth(w)
	p.tbl.SetHeight(h)
}

func (p *peersTab) Selected() string {
	r := p.tbl.SelectedRow()
	if r == nil {
		return ""
	}
	return r[0]
}

func (p *peersTab) SelectedName() string { return p.Selected() }

func (p *peersTab) Refresh(st transport.StatusResponse, selfID string) {
	rows := make([]table.Row, 0, len(st.Peers))
	for _, peer := range st.Peers {
		lastSeen := "-"
		if !peer.LastSeen.IsZero() {
			lastSeen = peer.LastSeen.UTC().Format(time.RFC3339)
		}
		id := peer.NodeID
		if peer.NodeID == selfID {
			id = "* " + peer.NodeID
		}
		rows = append(rows, table.Row{id, peer.Advertise, livenessText(peer.Live), lastSeen})
	}
	p.tbl.SetRows(rows)
}

// checksTab — checks with state and effective alerts.
type checksTab struct {
	tbl table.Model
}

func newChecksTab() *checksTab {
	cols := []table.Column{
		{Title: "ID", Width: 38},
		{Title: "NAME", Width: 18},
		{Title: "STATE", Width: 12},
		{Title: "OK/TOTAL", Width: 10},
		{Title: "ALERTS", Width: 24},
		{Title: "DETAIL", Width: 40},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	t.SetStyles(tableStyles())
	return &checksTab{tbl: t}
}

func (c *checksTab) Update(msg tea.Msg) (tabModel, tea.Cmd) {
	var cmd tea.Cmd
	c.tbl, cmd = c.tbl.Update(msg)
	return c, cmd
}

func (c *checksTab) View() string { return c.tbl.View() }

func (c *checksTab) SetSize(w, h int) {
	c.tbl.SetWidth(w)
	c.tbl.SetHeight(h)
}

func (c *checksTab) Selected() string {
	r := c.tbl.SelectedRow()
	if r == nil {
		return ""
	}
	return r[0]
}

func (c *checksTab) SelectedName() string {
	r := c.tbl.SelectedRow()
	if r == nil {
		return ""
	}
	return r[1]
}

func (c *checksTab) Refresh(st transport.StatusResponse) {
	rows := make([]table.Row, 0, len(st.Checks))
	for _, ch := range st.Checks {
		okTotal := lipgloss.NewStyle().Render("0/0")
		if ch.Total > 0 {
			okTotal = lipgloss.NewStyle().Render(itoa(ch.OKCount) + "/" + itoa(ch.Total))
		}
		alerts := strings.Join(ch.Alerts, ",")
		if alerts == "" {
			alerts = "-"
		}
		rows = append(rows, table.Row{
			ch.CheckID, ch.Name, renderState(ch.State), okTotal, alerts, truncate(ch.Detail, 38),
		})
	}
	c.tbl.SetRows(rows)
}

// alertsTab — configured notification channels.
type alertsTab struct {
	tbl    table.Model
	alerts []alertRow
}

type alertRow struct {
	ID       string
	Name     string
	Type     string
	Default  bool
	HasTmpl  bool
	Endpoint string
}

func newAlertsTab() *alertsTab {
	cols := []table.Column{
		{Title: "ID", Width: 38},
		{Title: "NAME", Width: 16},
		{Title: "TYPE", Width: 10},
		{Title: "DEFAULT", Width: 8},
		{Title: "CUSTOM-MSG", Width: 11},
		{Title: "ENDPOINT", Width: 36},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	t.SetStyles(tableStyles())
	return &alertsTab{tbl: t}
}

func (a *alertsTab) Update(msg tea.Msg) (tabModel, tea.Cmd) {
	var cmd tea.Cmd
	a.tbl, cmd = a.tbl.Update(msg)
	return a, cmd
}

func (a *alertsTab) View() string { return a.tbl.View() }

func (a *alertsTab) SetSize(w, h int) {
	a.tbl.SetWidth(w)
	a.tbl.SetHeight(h)
}

func (a *alertsTab) Selected() string {
	r := a.tbl.SelectedRow()
	if r == nil {
		return ""
	}
	return r[0]
}

func (a *alertsTab) SelectedName() string {
	r := a.tbl.SelectedRow()
	if r == nil {
		return ""
	}
	return r[1]
}

// SelectedAlert returns the row metadata for the cursor, so the parent
// can flip the default flag without a roundtrip.
func (a *alertsTab) SelectedAlert() *alertRow {
	idx := a.tbl.Cursor()
	if idx < 0 || idx >= len(a.alerts) {
		return nil
	}
	cp := a.alerts[idx]
	return &cp
}

func (a *alertsTab) Refresh(alerts []alertRow) {
	a.alerts = alerts
	rows := make([]table.Row, 0, len(alerts))
	for _, r := range alerts {
		def := "-"
		if r.Default {
			def = "yes"
		}
		tmpl := "-"
		if r.HasTmpl {
			tmpl = "yes"
		}
		rows = append(rows, table.Row{r.ID, r.Name, r.Type, def, tmpl, truncate(r.Endpoint, 34)})
	}
	a.tbl.SetRows(rows)
}

func tableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(colorBorder).
		BorderBottom(true).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("230")).
		Background(colorAccent).
		Bold(true)
	return s
}

func livenessText(live bool) string {
	if live {
		return "live"
	}
	return "dead"
}

func itoa(i int) string {
	// avoid pulling fmt in the hot path of refresh
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max < 4 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
