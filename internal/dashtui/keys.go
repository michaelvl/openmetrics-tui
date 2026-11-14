package dashtui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/michaelvl/openmetrics-tui/internal/panel"
)

// snapshotMsgDuration is how long a save confirmation stays in the footer.
const snapshotMsgDuration = 3 * time.Second

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()
	// A focused input swallows the keyboard, digits and q included.
	if m.edit.kind != editNone {
		return m.handleEditKey(msg)
	}
	if key == "ctrl+c" {
		return m.quit()
	}
	if m.showHelp {
		if key == "?" || key == "esc" || key == "q" {
			m.showHelp = false
		}
		return nil
	}
	if m.logOpen {
		m.handleLogKey(key)
		return nil
	}

	switch key {
	case "q":
		return m.quit()
	case "?":
		m.showHelp = true
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if i := int(key[0] - '1'); i < len(m.d.Variables) {
			return m.startVarEdit(i)
		}
	case "tab", "shift+tab":
		if n := len(m.panels); n > 0 {
			delta := 1
			if key == "shift+tab" {
				delta = n - 1
			}
			m.active = (m.active + delta) % n
			m.follow = true
		}
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case " ", "enter":
		if p := m.activePanel(); p != nil {
			p.folded = !p.folded
			m.follow = true
		}
	case "z":
		fold := false
		for _, p := range m.panels {
			if !p.folded {
				fold = true
			}
		}
		for _, p := range m.panels {
			p.folded = fold
		}
		m.follow = true
	case "s":
		m.sortMode = m.sortMode.Next()
	case "/":
		return m.startFilterEdit()
	case "r":
		return tea.Batch(m.dispatchAll(true)...)
	case "p":
		m.paused = !m.paused
	case "+", "=", "-":
		next := stepInterval(m.interval, key != "-")
		if next != m.interval {
			// Restart the timer from now, so speeding up from a long interval
			// does not wait out the rest of it; the old chain dies on its tick.
			m.interval = next
			m.tickGen++
			return m.tickCmd()
		}
	case "S":
		return m.saveSnapshot()
	case "L":
		if m.backend != nil {
			m.logOpen = true
			m.logScroll = 0
			return m.logsCmd()
		}
	case "R":
		if m.backend != nil && !m.restarting {
			m.restarting = true
			m.restartErr = ""
			return m.restartCmd()
		}
	case "pgup":
		m.scroll -= m.bodyHeight()
	case "pgdown":
		m.scroll += m.bodyHeight()
	case "home":
		m.scroll = 0
	case "end":
		m.scroll = len(m.lines)
	}
	return nil
}

func (m *Model) quit() tea.Cmd {
	m.cancel()
	return tea.Quit
}

func (m *Model) activePanel() *panelState {
	if m.active < 0 || m.active >= len(m.panels) {
		return nil
	}
	return m.panels[m.active]
}

// moveCursor moves the row cursor of the active panel. On a folded panel there
// are no rows, so the keys move between panels instead.
func (m *Model) moveCursor(delta int) {
	p := m.activePanel()
	if p == nil {
		return
	}
	m.follow = true
	if p.folded {
		if next := m.active + delta; next >= 0 && next < len(m.panels) {
			m.active = next
		}
		return
	}
	p.cursor = max(0, min(p.cursor+delta, len(p.shown)-1))
}

func (m *Model) handleLogKey(key string) {
	page := max(1, m.bodyHeight()-2)
	switch key {
	case "L", "esc", "q":
		m.logOpen = false
	case "up", "k":
		m.logScroll++
	case "down", "j":
		m.logScroll--
	case "pgup":
		m.logScroll += page
	case "pgdown":
		m.logScroll -= page
	case "home":
		m.logScroll = len(m.logs)
	case "end":
		m.logScroll = 0
	}
	m.logScroll = max(0, min(m.logScroll, max(0, len(m.logs)-1)))
}

// sortName is the footer's name for a sort mode.
func sortName(s panel.SortMode) string {
	switch s {
	case panel.SortNowDesc:
		return "now ↓"
	case panel.SortNowAsc:
		return "now ↑"
	case panel.SortLabel:
		return "label"
	}
	return "panel"
}

// saveSnapshot writes the header, every row of every panel (not just what is
// scrolled into view) and the footer to a timestamped plain-text file, with
// colour stripped, as the existing tool does.
func (m *Model) saveSnapshot() tea.Cmd {
	m.snapshotGen++
	gen := m.snapshotGen

	now := m.now()
	name := fmt.Sprintf("omtui-dash-snapshot-%s.txt", now.Format("20060102-150405"))
	path := filepath.Join(m.snapDir, name)
	preamble := fmt.Sprintf("# omtui-dash snapshot of %q, taken %s\n\n", m.d.Title, now.Format(time.RFC3339))
	screen := m.renderHeader() + "\n" + strings.Join(m.lines, "\n") + "\n" + m.renderFooter("")
	content := ansi.Strip(screen)

	if err := os.WriteFile(path, []byte(preamble+content+"\n"), 0o644); err != nil {
		m.snapshotMsg, m.snapshotErr = "snapshot failed: "+err.Error(), true
	} else {
		m.snapshotMsg, m.snapshotErr = "saved "+path, false
	}
	return m.tickFn(snapshotMsgDuration, func(time.Time) tea.Msg { return clearSnapshotMsg{gen: gen} })
}
