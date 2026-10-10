package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/pomesaka/claude-deck/internal/config"
)

var (
	// Color palette
	colorPrimary     = lipgloss.Color("#7C3AED") // purple
	colorSecondary   = lipgloss.Color("#06B6D4") // cyan
	colorSuccess     = lipgloss.Color("#10B981") // green
	colorWarning     = lipgloss.Color("#F59E0B") // amber
	colorDanger      = lipgloss.Color("#EF4444") // red
	colorBgSelected  = lipgloss.Color("#313244") // selected bg
	colorBorderFocus = lipgloss.Color("#7C3AED") // focused border
	colorText        = lipgloss.Color("#CDD6F4") // light text
	colorTextDim     = lipgloss.Color("#6C7086") // dim text

	// Session list styles
	sessionListStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorBorderFocus).
				Padding(0, 1)

	sessionItemStyle = lipgloss.NewStyle().
				Padding(0, 1)

	sessionItemSelectedStyle = lipgloss.NewStyle().
					Background(colorBgSelected).
					Padding(0, 1)

	// Status badge styles
	statusRunningStyle  = lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
	statusIdleStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#808898")).Bold(true)
	statusSubagentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#60A5FA")).Bold(true)
	statusApproveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#C08552")).Bold(true)
	statusQuestionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#C08552")).Bold(true)
	statusDoneStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#333346"))
	statusErrorStyle    = lipgloss.NewStyle().Foreground(colorDanger).Bold(true)
	unmanagedIconStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#333346"))

	// Footer / help
	footerStyle = lipgloss.NewStyle().
			Foreground(colorTextDim).
			Padding(0, 1)

	// Title
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorText)

	// Dim text
	dimStyle = lipgloss.NewStyle().
			Foreground(colorTextDim)

	// User message (subtle background highlight)
	userMessageStyle = lipgloss.NewStyle().
				Background(colorBgSelected).
				Foreground(colorText)

	// Tool name label
	toolNameStyle = lipgloss.NewStyle().Bold(true)

	// Tool icon styles (result status)
	toolDoneStyle    = lipgloss.NewStyle().Foreground(colorSuccess) // ✓ completed
	toolPendingStyle = lipgloss.NewStyle().Foreground(colorWarning) // ● pending/no result

	// Thinking indicator
	thinkingStyle = lipgloss.NewStyle().Foreground(colorTextDim).Italic(true)

	// Diff styles (unified diff coloring)
	diffAddStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E3A1")) // green
	diffDelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#F38BA8")) // red
	diffHunkStyle = lipgloss.NewStyle().Foreground(colorSecondary)            // cyan
)

// InitStyles rebuilds all style variables from the given theme configuration.
// Call this before NewModel to apply user-customized colors.
func InitStyles(theme config.ThemeConfig) {
	colorPrimary = lipgloss.Color(theme.Primary)
	colorSecondary = lipgloss.Color(theme.Secondary)
	colorSuccess = lipgloss.Color(theme.Success)
	colorWarning = lipgloss.Color(theme.Warning)
	colorDanger = lipgloss.Color(theme.Danger)
	colorBgSelected = lipgloss.Color(theme.BgSelected)
	colorBorderFocus = lipgloss.Color(theme.BorderFocus)
	colorText = lipgloss.Color(theme.Text)
	colorTextDim = lipgloss.Color(theme.TextDim)

	sessionListStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorBorderFocus).
		Padding(0, 1)

	sessionItemStyle = lipgloss.NewStyle().
		Padding(0, 1)

	sessionItemSelectedStyle = lipgloss.NewStyle().
		Background(colorBgSelected).
		Padding(0, 1)

	statusRunningStyle = lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
	statusIdleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(theme.StatusIdle)).Bold(true)
	statusSubagentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(theme.StatusSubagent)).Bold(true)
	statusApproveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(theme.StatusAttention)).Bold(true)
	statusQuestionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(theme.StatusAttention)).Bold(true)
	statusDoneStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(theme.StatusDone))
	statusErrorStyle = lipgloss.NewStyle().Foreground(colorDanger).Bold(true)
	unmanagedIconStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(theme.StatusDone))

	footerStyle = lipgloss.NewStyle().
		Foreground(colorTextDim).
		Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(colorText)

	dimStyle = lipgloss.NewStyle().
		Foreground(colorTextDim)

	userMessageStyle = lipgloss.NewStyle().
		Background(colorBgSelected).
		Foreground(colorText)

	toolDoneStyle = lipgloss.NewStyle().Foreground(colorSuccess)
	toolPendingStyle = lipgloss.NewStyle().Foreground(colorWarning)

	thinkingStyle = lipgloss.NewStyle().Foreground(colorTextDim).Italic(true)

	diffAddStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(theme.DiffAdd))
	diffDelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(theme.DiffDel))
	diffHunkStyle = lipgloss.NewStyle().Foreground(colorSecondary)
}
