package cli

import (
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// formTheme is huh's default theme (Charm) with the focused-field left
// border brightened — the stock color (ANSI 238) is nearly invisible on
// dark terminal backgrounds.
func formTheme() *huh.Theme {
	t := huh.ThemeCharm()
	t.Focused.Base = t.Focused.Base.BorderForeground(lipgloss.Color("245"))
	return t
}
