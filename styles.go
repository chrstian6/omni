package main

import "github.com/charmbracelet/lipgloss"

// Adaptive palette: lipgloss picks the Light or Dark value from the terminal's
// detected background, so Omni reads correctly in a light or dark terminal (and
// follows the system theme when the terminal does). Dark values are the warm
// near-black / cream / coral of the Mac HUD; light values invert to dark text
// on a warm off-white.
var (
	colBG      = lipgloss.AdaptiveColor{Light: "#FBFAF6", Dark: "#1F1E1C"}
	colSurface = lipgloss.AdaptiveColor{Light: "#ECEAE1", Dark: "#262624"}
	colBorder  = lipgloss.AdaptiveColor{Light: "#D7D3C8", Dark: "#3A3937"}
	colText    = lipgloss.AdaptiveColor{Light: "#2A2925", Dark: "#F0EEE6"}
	colMuted   = lipgloss.AdaptiveColor{Light: "#6E6B63", Dark: "#8F8D86"}
	colAccent  = lipgloss.AdaptiveColor{Light: "#BE5330", Dark: "#D97757"} // Claude coral
	colBusy    = lipgloss.AdaptiveColor{Light: "#2E7D4F", Dark: "#4FA46A"} // green reads as "running"
	colDanger  = lipgloss.AdaptiveColor{Light: "#B23B2A", Dark: "#CC5C4A"}

	styleTitle  = lipgloss.NewStyle().Foreground(colText).Bold(true)
	styleMuted  = lipgloss.NewStyle().Foreground(colMuted)
	styleAccent = lipgloss.NewStyle().Foreground(colAccent)
	styleBusy   = lipgloss.NewStyle().Foreground(colBusy)
	styleDanger = lipgloss.NewStyle().Foreground(colDanger)

	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder)

	styleSelectedRow = lipgloss.NewStyle().
				Background(colSurface).
				Foreground(colText).
				Bold(true)

	styleRow = lipgloss.NewStyle().Foreground(colText)

	styleHeader = lipgloss.NewStyle().
			Foreground(colBG).
			Background(colAccent).
			Bold(true).
			Padding(0, 1)

	styleFooter = lipgloss.NewStyle().Foreground(colMuted)

	styleBadge = lipgloss.NewStyle().
			Foreground(colBG).
			Background(colMuted).
			Padding(0, 1)

	styleBorder2 = lipgloss.NewStyle().Foreground(colBorder)
)

func dot(color lipgloss.TerminalColor) string {
	return lipgloss.NewStyle().Foreground(color).Render("●")
}
