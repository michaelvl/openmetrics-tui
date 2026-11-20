package dashtui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// View draws the header, the visible slice of the panel area and the footer.
func (m *Model) View() string {
	if m.showHelp {
		return m.renderHelp()
	}
	body := m.bodyView()
	hints := ""
	switch h := m.bodyHeight(); {
	case m.scroll > 0 && m.scroll+h < len(m.lines):
		hints = m.th.faint.Render(" ▲▼")
	case m.scroll > 0:
		hints = m.th.faint.Render(" ▲")
	case m.scroll+h < len(m.lines):
		hints = m.th.faint.Render(" ▼")
	}
	return m.renderHeader() + "\n" + strings.Join(body, "\n") + "\n" + m.renderFooter(hints)
}

// bodyView is exactly the rows between header and footer: any banner, then
// either the log overlay or the scrolled panel lines with the suggestion list
// on top.
func (m *Model) bodyView() []string {
	h := m.bodyHeight()
	body := append([]string(nil), m.bannerLines...)
	if m.logOpen {
		body = append(body, m.logView(h)...)
	} else {
		end := min(m.scroll+h, len(m.lines))
		panels := append([]string(nil), m.lines[min(m.scroll, end):end]...)
		for len(panels) < h {
			panels = append(panels, "")
		}
		body = append(body, m.overlaySuggestions(panels)...)
	}
	return body
}

// banner is the notice shown above the panels when a managed child has died or
// a restart is under way: what happened, the keys, and the last log lines.
func (m *Model) banner() []string {
	if m.backend == nil {
		return nil
	}
	var out []string
	switch {
	case m.restarting:
		out = append(out, m.th.warn.Render("↻ restarting the backend..."))
	case m.restartErr != "":
		out = append(out, m.th.err.Render(oneLine("restart failed: "+m.restartErr, m.width-2)))
	default:
		for _, st := range m.health {
			if !st.Running {
				why := "exited"
				if st.Err != nil {
					why += ": " + st.Err.Error()
				}
				out = append(out, m.th.err.Render(oneLine(fmt.Sprintf("⚠ %s %s", st.Name, why), m.width-2)))
				break
			}
		}
		if len(out) == 0 {
			return nil
		}
		out[0] += m.th.faint.Render("  R restart, L logs")
	}
	if logs := m.backend.Logs(); len(logs) > 0 {
		for _, l := range logs[max(0, len(logs)-2):] {
			out = append(out, m.th.faint.Render("  "+oneLine(l, m.width-4)))
		}
	}
	for i := range out {
		out[i] = clip(out[i], m.width)
	}
	return out
}

// logView shows the newest child output that fits, scrolled back by logScroll.
func (m *Model) logView(h int) []string {
	out := []string{m.th.faint.Render(fmt.Sprintf(" Backend log, %d lines  ↑↓ scroll  L/Esc close", len(m.logs)))}
	room := max(0, h-1)
	end := max(0, len(m.logs)-m.logScroll)
	start := max(0, end-room)
	for _, l := range m.logs[start:end] {
		out = append(out, clip(" "+l, m.width))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

// snapshotHelpLine is the help line for S, dropped when snapshots are off.
const snapshotHelpLine = "  S           Save the whole dashboard to a plain-text snapshot file\n"

const helpText = `
Help

  q/ctrl+c    Quit
  ?           Toggle this help
  1-9         Edit variable $n (Enter apply, Esc cancel, Tab next box)
              Typing filters the suggestions; up/down select, Tab/Enter accept
  tab         Next panel (shift+tab: previous)
  up/down     Previous/next panel (scrolls a panel taller than the screen first)
  space/enter Fold or unfold the active panel
  z           Fold all panels, or unfold all when all are folded
  s           Cycle sort: panel setting, now descending, now ascending, label
  /           Filter rows by substring (Enter keep, Esc cancel)
  r           Refresh now
  p           Pause/unpause refreshing
  +/-         Refresh faster / slower
  S           Save the whole dashboard to a plain-text snapshot file
  L           Show the managed backend log (managed mode)
  R           Restart the managed backend (managed mode)
  pgup/pgdn   Scroll the panel area

Press ? to close
`

func (m *Model) renderHelp() string {
	text := helpText
	if m.noSnap {
		text = strings.Replace(text, snapshotHelpLine, "", 1)
	}
	box := m.th.box().
		Padding(1, 2).
		Background(lipgloss.Color("235")).
		Foreground(lipgloss.Color("252")).
		Render(text)
	return m.th.r.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceChars(" "), lipgloss.WithWhitespaceForeground(lipgloss.Color("0")))
}
