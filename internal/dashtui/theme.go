package dashtui

import "github.com/charmbracelet/lipgloss"

// theme holds the styles of one session. Every style is built from the
// session's renderer, so its colour profile follows that session's terminal
// and not the process's stdout.
type theme struct {
	r *lipgloss.Renderer

	faint, err, warn, ok, accent, title, dashTitle lipgloss.Style
}

func newTheme(r *lipgloss.Renderer) *theme {
	return &theme{
		r:         r,
		faint:     r.NewStyle().Faint(true),
		err:       r.NewStyle().Foreground(lipgloss.Color(colRed)),
		warn:      r.NewStyle().Foreground(lipgloss.Color(colYellow)),
		ok:        r.NewStyle().Foreground(lipgloss.Color(colGreen)),
		accent:    r.NewStyle().Foreground(lipgloss.Color("63")),
		title:     r.NewStyle().Bold(true),
		dashTitle: r.NewStyle().Bold(true).Foreground(lipgloss.Color(colTitle)),
	}
}

// color maps a threshold colour name to a style. Names outside the usual
// three are passed to lipgloss as they are, which accepts ANSI numbers and hex.
func (t *theme) color(name string) lipgloss.Style {
	c := map[string]string{
		"red": colRed, "yellow": colYellow, "green": colGreen, "orange": "#ff9e64",
		"blue": "33", "purple": "135", "cyan": "86", "grey": "244", "gray": "244",
	}[name]
	if c == "" {
		c = name
	}
	return t.r.NewStyle().Foreground(lipgloss.Color(c))
}

// box is a rounded border in the accent colour.
func (t *theme) box() lipgloss.Style {
	return t.r.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63"))
}
