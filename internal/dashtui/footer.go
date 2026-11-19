package dashtui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/michaelvl/openmetrics-tui/internal/textutil"
)

const footerSep = " | "

// failing counts the panels whose last query failed.
func (m *Model) failing() int {
	n := 0
	for _, p := range m.panels {
		if p.err != nil {
			n++
		}
	}
	return n
}

func (m *Model) loading() bool {
	for _, p := range m.panels {
		if !p.loaded {
			return true
		}
	}
	return false
}

// statusSegment is the footer's right end: overall panel health.
func (m *Model) statusSegment() string {
	switch n := m.failing(); {
	case !m.healthy():
		return m.th.err.Render("⚠ backend down")
	case n > 0:
		return m.th.err.Render(fmt.Sprintf("⚠ %d of %d panels failing", n, len(m.panels)))
	case m.loading():
		return m.th.faint.Render("● loading")
	}
	return m.th.ok.Render("●")
}

// allOK reports whether statusSegment shows the plain green dot.
func (m *Model) allOK() bool {
	return m.healthy() && m.failing() == 0 && !m.loading()
}

// renderFooter builds the one-line footer, in the existing tool's `Name (key)`
// style. scrollHints is passed in so a snapshot, which lists everything, can
// pass none.
func (m *Model) renderFooter(scrollHints string) string {
	if m.edit.kind != editNone {
		return clip(m.editFooter(), m.width)
	}
	status := m.statusSegment()
	target := m.url
	if m.kubeCtx != "" {
		target = "kubectl context: " + m.kubeCtx
	}
	switch {
	case target == "":
		if m.allOK() {
			status += " ok"
		}
	case m.allOK():
		// Like omtui: the dot alone says ok, the target follows it directly.
		status += " " + m.th.faint.Render(target)
	default:
		status += footerSep + m.th.faint.Render(target)
	}
	status += scrollHints

	left := "? for help"
	if m.snapshotMsg != "" {
		style := m.th.faint
		if m.snapshotErr {
			style = m.th.err
		}
		left = style.Render(textutil.TruncateToWidth(m.snapshotMsg, max(12, m.width/2)))
	}
	segs := []string{left}
	if m.paused {
		segs = append(segs, m.th.warn.Bold(true).Render("⏸ PAUSED (p)"))
	}
	// The restart key goes early: it is the one to find when something broke.
	if m.backend != nil && !m.healthy() {
		segs = append(segs, "Restart (R)")
	}
	segs = append(segs, "Fold (space)", "Sort (s): "+sortName(m.sortMode))
	if m.rowFilter != "" {
		segs = append(segs, "Filter (/): "+m.rowFilter)
	} else {
		segs = append(segs, "Filter (/)")
	}
	if m.backend != nil {
		segs = append(segs, "Log (L)")
	}
	// Like omtui, the interval sits just before the status, since it says how
	// often the target is scraped.
	segs = append(segs, "⟳ "+formatInterval(m.interval)+" (+/-)")
	// Segments that do not fit are dropped from the right, the status stays.
	for len(segs) > 1 && lipgloss.Width(strings.Join(segs, footerSep)+footerSep+status) > m.width {
		segs = segs[:len(segs)-1]
	}
	return clip(strings.Join(segs, footerSep)+footerSep+status, m.width)
}

func (m *Model) healthy() bool {
	for _, st := range m.health {
		if !st.Running {
			return false
		}
	}
	return true
}

// editFooter is the footer while the input is open: the key hints, then the
// validation error or the suggestion count.
func (m *Model) editFooter() string {
	if m.edit.kind == editFilter {
		return "Filter (/): " + m.input.View() + footerSep + m.th.faint.Render("Enter keep  Esc cancel")
	}
	list := m.filtered()
	hint := "Enter apply  Esc cancel  Tab next"
	switch {
	case m.edit.hl >= 0 && len(list) > 0:
		hint = "↑↓ select  Tab/Enter accept  Esc cancel"
	case len(list) > 0:
		hint = "Enter apply  Esc cancel  Tab next  ↓ options"
	}
	var info string
	switch {
	case m.edit.err != "":
		info = m.th.err.Render(m.edit.err)
	case m.edit.sug == sugLoading:
		info = m.th.faint.Render("loading values…")
	case m.edit.sug == sugReady:
		info = fmt.Sprintf("%d of %d values", len(list), len(m.edit.all))
		if !slices.Contains(m.edit.all, m.input.Value()) {
			info += m.th.faint.Render(", not in list")
		}
	default:
		info = m.th.faint.Render("no value list, free text")
	}
	return hint + " │ " + info
}
