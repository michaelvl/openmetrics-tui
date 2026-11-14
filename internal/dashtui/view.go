package dashtui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/prometheus/common/model"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/panel"
	"github.com/michaelvl/openmetrics-tui/internal/textutil"
)

const (
	// lead is the width of the left margin, gutter and space before a row title.
	lead = 3
	// colSep separates grid columns.
	colSep = 2
	// minColW is the narrowest a window column is drawn.
	minColW = 6
	// trendW is the sparkline width.
	trendW = 12
	// minLabelW is the narrowest the row title column shrinks to.
	minLabelW = 8
	// noDataMsg is shown for a panel whose query succeeded with no series.
	noDataMsg = "no data (not scraped?)"
)

// Status colours, sampled from the Tokyo Night palette: softer than the
// saturated ANSI 196/220 on a dark background.
const (
	colRed    = "#f7768e"
	colYellow = "#e0af68"
	colGreen  = "#9ece6a"
	colTitle  = "#7aa2f7" // Tokyo Night blue, for the dashboard title
)

var (
	faintStyle     = lipgloss.NewStyle().Faint(true)
	errStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color(colRed))
	warnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color(colYellow))
	okStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color(colGreen))
	accentStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	titleStyle     = lipgloss.NewStyle().Bold(true)
	dashTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colTitle))
)

// colorStyle maps a threshold colour name to a style. Names outside the usual
// three are passed to lipgloss as they are, which accepts ANSI numbers and hex.
func colorStyle(name string) lipgloss.Style {
	c := map[string]string{
		"red": colRed, "yellow": colYellow, "green": colGreen, "orange": "#ff9e64",
		"blue": "33", "purple": "135", "cyan": "86", "grey": "244", "gray": "244",
	}[name]
	if c == "" {
		c = name
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c))
}

// bodyHeight is the rows available to panels: everything but the header, the
// footer and any banner.
func (m *Model) bodyHeight() int {
	return max(1, m.height-headerHeight-1-len(m.bannerLines))
}

// grid is the column layout shared by every expanded panel, so columns line
// up from one panel to the next.
type grid struct {
	labelW int
	cols   []int // indices of the shown windows
	colW   []int // parallel to cols
	trend  int   // sparkline width, 0 when dropped
}

func (g grid) width() int {
	w := lead + g.labelW
	for _, cw := range g.colW {
		w += colSep + cw
	}
	if g.trend > 0 {
		w += colSep + 1 + g.trend
	}
	return w
}

// cell is a formatted window cell.
type cell struct {
	text  string
	color string // threshold colour, "" for none
}

// width includes the marker column that follows a coloured cell.
func (c cell) width() int {
	w := lipgloss.Width(c.text)
	if c.color != "" {
		w++
	}
	return w
}

func (m *Model) cellFor(p *panelState, c panel.Cell) cell {
	switch {
	case c.Before:
		return cell{text: "·"}
	case math.IsNaN(c.V):
		return cell{text: "-"}
	}
	return cell{text: panel.FormatValue(p.def.Unit, c.V), color: panel.Color(p.def.Thresholds, c.V)}
}

// computeGrid sizes the shared columns for width columns of terminal. Too wide
// a grid first loses the sparkline, then the longest windows one at a time
// (never the now column), then shortens the row titles.
func (m *Model) computeGrid() grid {
	g := grid{trend: 0}
	if m.d.Spark > 0 {
		g.trend = trendW
	}
	g.labelW = minLabelW
	winW := make([]int, len(m.d.Windows))
	for i, w := range m.d.Windows {
		winW[i] = max(minColW, lipgloss.Width(w.Label))
	}
	for _, p := range m.panels {
		if p.folded || p.err != nil {
			continue
		}
		for _, r := range p.shown {
			g.labelW = max(g.labelW, lipgloss.Width(r.Title))
			for i, c := range r.Cells {
				winW[i] = max(winW[i], m.cellFor(p, c).width())
			}
		}
	}
	// Row titles never take more than half the terminal, so the grid still has
	// room on a normal width.
	g.labelW = min(g.labelW, max(minLabelW, m.width/2))
	shown := make([]bool, len(m.d.Windows))
	for i := range shown {
		shown[i] = true
	}
	build := func() {
		g.cols, g.colW = nil, nil
		for i := range shown {
			if shown[i] {
				g.cols = append(g.cols, i)
				g.colW = append(g.colW, winW[i])
			}
		}
	}
	build()
	if g.width() > m.width {
		g.trend = 0
	}
	for g.width() > m.width {
		longest := -1
		for i, w := range m.d.Windows {
			if shown[i] && w.Offset > 0 && (longest < 0 || w.Offset > m.d.Windows[longest].Offset) {
				longest = i
			}
		}
		if longest < 0 {
			break
		}
		shown[longest] = false
		build()
	}
	if over := g.width() - m.width; over > 0 {
		g.labelW = max(minLabelW, g.labelW-over)
	}
	return g
}

// title is the title row text: the title, the unit in parentheses and the row
// limit.
func (p *panelState) title() string {
	t := p.def.Title
	switch p.def.Unit {
	case "s", "/min":
		t += " (" + p.def.Unit + ")"
	}
	if p.def.Limit > 0 {
		t += fmt.Sprintf(", top %d", p.def.Limit)
	}
	return t
}

// thresholdLabel is the comparison text of one rule: ">0.5", "≥3" or "else".
func (p *panelState) thresholdLabel(t dashboard.Threshold) string {
	v := panel.FormatValue(p.def.Unit, t.Value)
	switch t.Op {
	case "gt":
		return ">" + v
	case "gte":
		return "≥" + v
	case "lt":
		return "<" + v
	case "lte":
		return "≤" + v
	}
	return "else"
}

// legend is the threshold legend for the title row: a coloured marker and the
// comparison for each rule, in rule order (first match wins).
func (p *panelState) legend() string {
	var parts []string
	for _, t := range p.def.Thresholds {
		parts = append(parts, colorStyle(t.Color).Render("●")+p.thresholdLabel(t))
	}
	return strings.Join(parts, "  ")
}

// coldNote returns the "collecting data" note when the stored history is
// shorter than the longest window, or "". The earliest sample across the panel
// says how much history exists.
func (m *Model) coldNote(p *panelState) string {
	if len(p.rows) == 0 || p.rng.End.IsZero() {
		return ""
	}
	var longest time.Duration
	for _, w := range m.d.Windows {
		longest = max(longest, w.Offset)
	}
	earliest := panel.Earliest(p.rows)
	if earliest.IsZero() || !earliest.After(p.rng.End.Add(-longest).Add(p.rng.Step/2)) {
		return ""
	}
	return fmt.Sprintf("collecting data (%d min)", int(p.rng.End.Sub(earliest)/time.Minute))
}

// rebuild recomputes the shown rows, the line cache, the banner and the
// scroll position. Called after every update.
func (m *Model) rebuild() {
	m.bannerLines = m.banner()
	for _, p := range m.panels {
		arranged := panel.Arrange(p.rows, p.def, m.d.Windows, m.sortMode)
		p.shown = arranged
		if m.rowFilter != "" {
			needle := strings.ToLower(m.rowFilter)
			p.shown = nil
			for _, r := range arranged {
				if strings.Contains(strings.ToLower(r.Title), needle) {
					p.shown = append(p.shown, r)
				}
			}
		}
		for i := range p.shown {
			if p.shown[i].Title == "{}" {
				p.shown[i].Title = "(single series)" // a query with no labels
			}
		}
		p.cursor = max(0, min(p.cursor, len(p.shown)-1))
	}
	m.lines, m.titleLine, m.cursorLine = m.buildLines()

	h := m.bodyHeight()
	if m.follow && len(m.titleLine) > 0 {
		top := m.titleLine[m.active]
		bottom := top
		if m.cursorLine >= 0 {
			bottom = m.cursorLine
		}
		if bottom-top+1 <= h {
			m.scroll = max(min(m.scroll, top), bottom-h+1)
		} else {
			m.scroll = max(min(m.scroll, bottom), bottom-h+1)
		}
	}
	m.follow = false
	m.scroll = max(0, min(m.scroll, len(m.lines)-h))
}

func (m *Model) buildLines() (lines []string, titleLine []int, cursorLine int) {
	cursorLine = -1
	g := m.computeGrid()
	m.shownWindows = g.cols
	rule := faintStyle.Render(" " + strings.Repeat("─", max(0, m.width-2)))
	lines = append(lines, rule)

	tw := 0
	for _, p := range m.panels {
		tw = max(tw, lipgloss.Width(p.title()))
	}
	tw = min(tw, max(10, m.width/2))

	headW, countsW := m.foldedWidths()
	for i, p := range m.panels {
		active := i == m.active
		titleLine = append(titleLine, len(lines))
		if p.folded {
			lines = append(lines, m.foldedLine(p, active, tw, headW, countsW))
			continue
		}
		lines = append(lines, m.titleRow(p, active))
		switch {
		case p.err != nil:
			lines = append(lines, "   "+errStyle.Render(oneLine(p.err.Error(), m.width-4)))
		case !p.loaded:
			lines = append(lines, "   "+faintStyle.Render("loading…"))
		case len(p.rows) == 0:
			lines = append(lines, "   "+warnStyle.Render(noDataMsg))
		case len(p.shown) == 0:
			lines = append(lines, "   "+faintStyle.Render("no rows match the filter"))
		default:
			lines = append(lines, m.columnHeader(g))
			for ri, r := range p.shown {
				if active && ri == p.cursor {
					cursorLine = len(lines)
				}
				lines = append(lines, m.rowLine(p, r, g, active, ri == p.cursor))
			}
		}
		lines = append(lines, rule)
	}
	for i, l := range lines {
		lines[i] = clip(l, m.width)
	}
	return lines, titleLine, cursorLine
}

// clip cuts a styled line to width cells.
func clip(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// oneLine collapses whitespace and cuts to width.
func oneLine(s string, width int) string {
	return textutil.TruncateToWidth(strings.Join(strings.Fields(s), " "), max(1, width))
}

func padRight(s string, w int) string {
	if gap := w - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

func (m *Model) titleRow(p *panelState, active bool) string {
	left := activeCue(active) + "▾ " + styleTitle(p.title(), active)
	var right []string
	if note := m.coldNote(p); note != "" {
		right = append(right, warnStyle.Render(note))
	}
	if leg := p.legend(); leg != "" && p.err == nil {
		right = append(right, leg)
	}
	return joinEnds(left, right, m.width)
}

// activeCue is the first column of a title row: a bar for the active panel. It
// is a character, not just a colour, so it survives colourless output.
func activeCue(active bool) string {
	if active {
		return accentStyle.Render("▌")
	}
	return " "
}

func styleTitle(t string, active bool) string {
	if active {
		return accentStyle.Inherit(titleStyle).Render(t)
	}
	return titleStyle.Render(t)
}

// joinEnds puts left at the start of a line and as many of the right-hand
// parts as fit at the end, dropping the last one first.
func joinEnds(left string, right []string, width int) string {
	for len(right) > 0 {
		r := strings.Join(right, "  ")
		if gap := width - lipgloss.Width(left) - lipgloss.Width(r) - 1; gap >= 2 {
			return left + strings.Repeat(" ", gap) + r
		}
		right = right[:len(right)-1]
	}
	return left
}

func (m *Model) columnHeader(g grid) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", lead) + strings.Repeat(" ", g.labelW))
	for k, wi := range g.cols {
		b.WriteString(strings.Repeat(" ", colSep) + padRight(m.d.Windows[wi].Label, g.colW[k]))
	}
	if g.trend > 0 {
		b.WriteString(strings.Repeat(" ", colSep+1) + "trend (" + model.Duration(m.d.Spark).String() + ")")
	}
	return faintStyle.Render(b.String())
}

func (m *Model) rowLine(p *panelState, r panel.Row, g grid, active, cursor bool) string {
	gutter := " "
	if active {
		gutter = faintStyle.Render("│")
		if cursor {
			gutter = accentStyle.Render("▌")
		}
	}
	label := padRight(textutil.TruncateToWidth(r.Title, g.labelW), g.labelW)
	if cursor && active {
		label = titleStyle.Render(label)
	}
	b := " " + gutter + " " + label
	for k, wi := range g.cols {
		c := m.cellFor(p, r.Cells[wi])
		txt, w := c.text, c.width()
		if c.color != "" {
			txt = colorStyle(c.color).Render(c.text + "●")
		}
		b += strings.Repeat(" ", colSep) + txt + strings.Repeat(" ", max(0, g.colW[k]-w))
	}
	if g.trend > 0 {
		spark := panel.Sparkline(r.Spark, g.trend)
		if nowColor := panel.Color(p.def.Thresholds, r.Now(m.d.Windows)); nowColor != "" {
			spark = colorStyle(nowColor).Render(spark)
		}
		b += strings.Repeat(" ", colSep+1) + spark
	}
	return b
}

// foldedWidths are the widest head and counts columns over the folded panels.
func (m *Model) foldedWidths() (headW, countsW int) {
	for _, p := range m.panels {
		if !p.folded {
			continue
		}
		if f := m.foldedSummary(p); f.counted {
			headW = max(headW, lipgloss.Width(f.head))
			countsW = max(countsW, lipgloss.Width(f.counts))
		}
	}
	return headW, countsW
}

// foldedParts is a folded panel's summary in up to three columns, so that
// panels can be aligned: the row total, the per-colour counts and the worst
// row. Summaries that are not a count (a single value, an error, loading) are
// all in head, with the other columns empty.
type foldedParts struct {
	head, counts, worst string
	counted             bool
}

// foldedLine is the one-line form of a panel: title, row total, per-colour
// row counts with their thresholds and the worst row, kept live by the
// panel's continued querying. headW and countsW align the columns across
// panels.
func (m *Model) foldedLine(p *panelState, active bool, tw, headW, countsW int) string {
	title := textutil.TruncateToWidth(p.title(), tw)
	left := activeCue(active) + "▸ " + styleTitle(padRight(title, tw), active)
	f := m.foldedSummary(p)
	if !f.counted {
		return left + "  " + f.head
	}
	return left + "  " + padRight(f.head, headW) + "  " + padRight(f.counts, countsW) + f.worst
}

func (m *Model) foldedSummary(p *panelState) foldedParts {
	switch {
	case p.err != nil:
		return foldedParts{head: errStyle.Render(oneLine(p.err.Error(), max(10, m.width-lead-30)))}
	case !p.loaded:
		return foldedParts{head: faintStyle.Render("loading…")}
	case len(p.rows) == 0:
		return foldedParts{head: warnStyle.Render(noDataMsg)}
	}
	rows := panel.Arrange(p.rows, p.def, m.d.Windows, panel.SortYAML)
	s := panel.Summarize(p.def, m.d.Windows, rows)
	switch {
	case s.Single:
		v := m.valueText(p, s.Value)
		if c := panel.Color(p.def.Thresholds, s.Value); c != "" {
			v = colorStyle(c).Render("● " + v)
		}
		// The row's name: with one row it is the only hint what is measured.
		if t := rows[0].Title; t != "" && t != "{}" {
			v += "  " + faintStyle.Render(t)
		}
		return foldedParts{head: v}
	case len(s.Counts) == 0:
		return foldedParts{head: fmt.Sprintf("%d rows", s.Rows)}
	}
	// A colour's label is its first rule's, which is the legend's.
	labels := map[string]string{}
	for _, t := range p.def.Thresholds {
		if _, ok := labels[t.Color]; !ok {
			labels[t.Color] = p.thresholdLabel(t)
		}
	}
	var out []string
	for _, c := range s.Counts {
		out = append(out, colorStyle(c.Color).Render("●")+fmt.Sprint(c.N)+" "+faintStyle.Render(labels[c.Color]))
	}
	f := foldedParts{head: fmt.Sprintf("%d rows", s.Rows), counts: strings.Join(out, "  "), counted: true}
	if s.Worst >= 0 {
		f.worst = "  worst: " + rows[s.Worst].Title + " " + colorStyle(s.WorstColor).Render(m.valueText(p, s.WorstValue))
	}
	return f
}

func (m *Model) valueText(p *panelState, v float64) string {
	return panel.FormatValue(p.def.Unit, v)
}
