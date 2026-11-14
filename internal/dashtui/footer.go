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
		return errStyle.Render("⚠ backend down")
	case n > 0:
		return errStyle.Render(fmt.Sprintf("⚠ %d of %d panels failing", n, len(m.panels)))
	case m.loading():
		return faintStyle.Render("● loading")
	}
	return okStyle.Render("●") + " ok"
}

// renderFooter builds the one-line footer, in the existing tool's `Name (key)`
// style. scrollHints is passed in so a snapshot, which lists everything, can
// pass none.
func (m *Model) renderFooter(scrollHints string) string {
	if m.edit.kind != editNone {
		return clip(m.editFooter(), m.width)
	}
	status := m.statusSegment()
	switch {
	case m.kubeCtx != "":
		status += footerSep + faintStyle.Render("kubectl context: "+m.kubeCtx)
	case m.url != "":
		status += footerSep + faintStyle.Render(m.url)
	}
	status += scrollHints

	left := "? for help"
	if m.snapshotMsg != "" {
		style := faintStyle
		if m.snapshotErr {
			style = errStyle
		}
		left = style.Render(textutil.TruncateToWidth(m.snapshotMsg, max(12, m.width/2)))
	}
	segs := []string{left, "⟳ " + formatInterval(m.interval) + " (+/-)"}
	if m.paused {
		segs = append(segs, warnStyle.Bold(true).Render("⏸ PAUSED (p)"))
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
		return "Filter (/): " + m.input.View() + footerSep + faintStyle.Render("Enter keep  Esc cancel")
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
		info = errStyle.Render(m.edit.err)
	case m.edit.sug == sugLoading:
		info = faintStyle.Render("loading values…")
	case m.edit.sug == sugReady:
		info = fmt.Sprintf("%d of %d values", len(list), len(m.edit.all))
		if !slices.Contains(m.edit.all, m.input.Value()) {
			info += faintStyle.Render(", not in list")
		}
	default:
		info = faintStyle.Render("no value list, free text")
	}
	return hint + " │ " + info
}
