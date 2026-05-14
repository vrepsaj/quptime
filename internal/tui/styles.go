package tui

import "github.com/charmbracelet/lipgloss"

var (
	colorBorder    = lipgloss.Color("63")  // soft purple
	colorAccent    = lipgloss.Color("212") // pink
	colorMuted     = lipgloss.Color("241") // gray
	colorSuccess   = lipgloss.Color("42")  // green
	colorWarn      = lipgloss.Color("214") // orange
	colorError     = lipgloss.Color("196") // red
	colorTabActive = lipgloss.Color("212")
	colorTabIdle   = lipgloss.Color("241")
)

var (
	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("230")).
			Background(colorAccent).
			Bold(true).
			Padding(0, 1)

	subtleStyle = lipgloss.NewStyle().Foreground(colorMuted)

	headerStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1)

	tabActiveStyle = lipgloss.NewStyle().
			Foreground(colorTabActive).
			Bold(true).
			Underline(true).
			Padding(0, 1)

	tabIdleStyle = lipgloss.NewStyle().
			Foreground(colorTabIdle).
			Padding(0, 1)

	bodyStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1)

	helpStyle = lipgloss.NewStyle().Foreground(colorMuted)

	flashInfoStyle  = lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
	flashErrorStyle = lipgloss.NewStyle().Foreground(colorError).Bold(true)
	flashWarnStyle  = lipgloss.NewStyle().Foreground(colorWarn).Bold(true)

	modalStyle = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colorAccent).
			Padding(1, 2)

	stateUpStyle      = lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
	stateDownStyle    = lipgloss.NewStyle().Foreground(colorError).Bold(true)
	stateUnknownStyle = lipgloss.NewStyle().Foreground(colorMuted)
)

func renderState(s string) string {
	switch s {
	case "up":
		return stateUpStyle.Render("● up")
	case "down":
		return stateDownStyle.Render("● down")
	default:
		return stateUnknownStyle.Render("○ unknown")
	}
}

func renderLive(live bool) string {
	if live {
		return stateUpStyle.Render("● live")
	}
	return stateDownStyle.Render("● dead")
}
