package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ViewMode selects which of the two top-level views is on screen. The accordion's
// expanded state is the histogram detail, so there is no separate third view.
type ViewMode int

const (
	ViewMetrics ViewMode = iota
	ViewDistributions
)

// distEntry pairs a family with its store signature, which is the stable key the
// expansion and zoom state are recorded under.
type distEntry struct {
	sig  string
	dist *DistributionSeries
}

// columnGap separates the hand-rendered columns. The distribution view draws no
// borders, so a gap wide enough to read as a column break takes their place.
const columnGap = 2

// blockIndent sets an expanded grid in from the family line that owns it, which
// is the only cue that the two belong together once the grid is several rows tall.
const blockIndent = "    "

// distHeaders label the collapsed line. Only the leading column is left-aligned.
var distHeaders = append([]string{"DISTRIBUTION", "COUNT", "RATE"}, displayQuantileNames...)

var faintStyle = lipgloss.NewStyle().Faint(true)

// nativeHistogramNote replaces the grid for a native (exponential) histogram.
// Those carry no classic buckets at all, so there is nothing to lay out - but a
// blank block would read as a bug rather than as an unsupported encoding.
const nativeHistogramNote = "native histogram - buckets not supported"

// shadeRamp steps from the quietest bucket to the busiest. Keeping it as one
// slice means the whole heatmap retunes in a single place; lipgloss degrades the
// colours itself on terminals that cannot show them. The brightest steps flip
// their foreground dark, because light text on a light background is unreadable.
var shadeRamp = []lipgloss.Style{
	lipgloss.NewStyle().Background(lipgloss.Color("17")),
	lipgloss.NewStyle().Background(lipgloss.Color("18")),
	lipgloss.NewStyle().Background(lipgloss.Color("20")),
	lipgloss.NewStyle().Background(lipgloss.Color("26")),
	lipgloss.NewStyle().Background(lipgloss.Color("32")),
	lipgloss.NewStyle().Background(lipgloss.Color("39")).Foreground(lipgloss.Color("232")),
	lipgloss.NewStyle().Background(lipgloss.Color("45")).Foreground(lipgloss.Color("232")),
}

// visibleDistributions returns the families the current filters admit, sorted by
// signature so the list order stays stable from one scrape to the next.
func (m model) visibleDistributions() []distEntry {
	sigs := make([]string, 0, len(m.store.Distributions))
	for sig := range m.store.Distributions {
		sigs = append(sigs, sig)
	}
	sort.Strings(sigs)

	entries := make([]distEntry, 0, len(sigs))
	for _, sig := range sigs {
		dist := m.store.Distributions[sig]
		if !m.matchesFilters(dist.Name, dist.Labels) {
			continue
		}
		entries = append(entries, distEntry{sig: sig, dist: dist})
	}

	// Filter, then aggregate, then hide static, in the order buildTable gives its
	// reasons for: folding a filtered-out family in would total series the filter
	// was asked to exclude, and judging staticness first would drop a member whose
	// group is not static - the sum over a quiet target and a busy one moves.
	entries = m.aggregateDistributions(entries)

	if m.cfg.HideStatic {
		kept := entries[:0]
		for _, entry := range entries {
			if entry.dist.IsStatic() {
				continue
			}
			kept = append(kept, entry)
		}
		entries = kept
	}
	return entries
}

// cursorSpan is the range of lines the distribution cursor occupies: its
// collapsed line, plus the grid inlined beneath it when the family is expanded.
// Scrolling works on the whole span rather than on the line alone, because
// revealing a family's name while leaving its own numbers below the bottom of
// the screen defeats the point of moving the cursor there.
type cursorSpan struct {
	start int
	end   int
}

// renderDistributions draws the distribution view and reports which lines the
// cursor landed on, so the caller can scroll them into view.
func (m model) renderDistributions() (string, cursorSpan) {
	entries := m.visibleDistributions()
	if len(entries) == 0 {
		return m.emptyDistributionsMessage(), cursorSpan{}
	}
	if m.zoomed != "" {
		for _, entry := range entries {
			if entry.sig == m.zoomed {
				return m.renderZoom(entry), cursorSpan{}
			}
		}
	}
	return m.renderAccordion(entries)
}

// renderAccordion draws the collapsed list with each expanded family's grid
// inlined beneath its own line.
//
// The collapsed lines share one column layout and the expanded grids share a
// second, computed across every open block at once: that is what makes a time
// column mean the same scrape all the way down the page, however much history
// each individual family happens to have retained.
func (m model) renderAccordion(entries []distEntry) (string, cursorSpan) {
	rows := make([][]string, len(entries))
	for i, entry := range entries {
		rows[i] = m.collapsedCells(entry)
	}
	widths := calculateColumnWidths(distHeaders, rows)
	widths[0] = m.fitNameColumn(widths)

	blocks := make(map[int]*distBlock)
	var open []*distBlock
	for i, entry := range entries {
		if !m.expanded[entry.sig] {
			continue
		}
		block := m.newDistBlock(entry)
		blocks[i] = block
		open = append(open, block)
	}
	var grid gridLayout
	if len(open) > 0 {
		grid = m.layOutGrid(open, len(blockIndent))
	}

	lines := []string{
		m.distributionHeader(entries),
		faintStyle.Render(renderRow(distHeaders, widths)),
	}
	cursor := cursorSpan{start: len(lines), end: len(lines)}
	for i := range entries {
		line := renderRow(rows[i], widths)
		if i == m.distCursor {
			cursor.start = len(lines)
			line = m.cursorStyle.Render(line)
		}
		lines = append(lines, line)
		if block, ok := blocks[i]; ok {
			lines = append(lines, m.renderBlock(block, blockIndent, grid)...)
		}
		if i == m.distCursor {
			// The span closes after the block, so an expanded family is scrolled
			// into view as one unit rather than as a line with a tail hanging off
			// the bottom of the screen.
			cursor.end = len(lines) - 1
		}
	}
	return strings.Join(lines, "\n"), cursor
}

// renderZoom spends the whole screen on one family. Dropping the other blocks is
// what buys the history depth: the bound column is sized to this family alone,
// the grid is no longer indented under a list, and nothing else competes for the
// rows. The derived stats come along as a line of their own so the zoomed grid
// still answers "how busy, how slow" without the list on screen.
func (m model) renderZoom(entry distEntry) string {
	block := m.newDistBlock(entry)
	grid := m.layOutGrid([]*distBlock{block}, 0)

	title := lipgloss.NewStyle().Bold(true).Render(entry.dist.Name + m.formatDistLabels(entry.dist))
	hint := faintStyle.Render(fmt.Sprintf("Buckets: %s (b) | esc: back", m.bucketMode))

	lines := []string{title + "  " + hint, m.zoomStats(entry.dist), ""}
	lines = append(lines, m.renderBlock(block, "", grid)...)
	return strings.Join(lines, "\n")
}

// zoomStats restates the collapsed line's derived numbers for the zoomed view,
// where that line is no longer on screen.
func (m model) zoomStats(dist *DistributionSeries) string {
	stats := distSummary(dist, distScrapeCount(dist)-1, m.cfg.Interval)
	var parts []string
	if stats.CountOK {
		parts = append(parts, "count "+formatCompact(stats.Count))
	}
	if stats.RateOK {
		parts = append(parts, "rate "+formatCompact(stats.Rate)+"/s")
	}
	for i, cell := range stats.Cells {
		parts = append(parts, displayQuantileNames[i]+" "+formatQuantile(cell))
	}
	return faintStyle.Render(strings.Join(parts, "  "))
}

// distBlock is one expanded family's grid, computed once so the layout pass and
// the render pass cannot disagree about any number.
type distBlock struct {
	dist   *DistributionSeries
	label  string      // heading for the leading column
	bounds []string    // one per row, in Points order
	rows   [][]float64 // parallel to bounds; trimmed to the visible columns
	max    float64     // largest positive value on screen, the shading reference
}

// gridLayout is the column layout every expanded block on screen shares.
type gridLayout struct {
	boundWidth int
	headers    []string
	widths     []int
}

// newDistBlock derives one family's display values under the current bucket and
// delta modes. Rows still span the family's whole retained history at this point;
// layOutGrid trims them to what fits.
func (m model) newDistBlock(entry distEntry) *distBlock {
	block := &distBlock{dist: entry.dist, label: "le"}
	if entry.dist.Kind == KindSummary {
		// A summary's rows are reported quantiles, not bucket bounds.
		block.label = "quantile"
	}
	block.rows = bucketDisplayValues(entry.dist, m.bucketMode, m.cfg.DeltaMode)
	for _, point := range entry.dist.Points {
		block.bounds = append(block.bounds, formatBound(point.Bound))
	}
	return block
}

// layOutGrid picks how many time columns fit and how wide each must be, then
// trims every block to exactly those columns.
//
// Columns are dropped from the left, so the newest scrape is always on screen and
// blocks stay right-aligned on "Curr" - a family that only started reporting a
// minute ago lines its history up under the same columns as one that has been
// running all along.
func (m model) layOutGrid(blocks []*distBlock, indent int) gridLayout {
	total := m.historyColumns()

	grid := gridLayout{headers: make([]string, total), widths: make([]int, total)}
	for j := range grid.headers {
		grid.headers[j] = fmt.Sprintf("-%ds", (total-1-j)*int(m.cfg.Interval.Seconds()))
	}
	grid.headers[total-1] = "Curr"

	for _, block := range blocks {
		if w := lipgloss.Width(block.label); w > grid.boundWidth {
			grid.boundWidth = w
		}
		for _, bound := range block.bounds {
			if w := lipgloss.Width(bound); w > grid.boundWidth {
				grid.boundWidth = w
			}
		}
	}

	for j := range grid.widths {
		grid.widths[j] = lipgloss.Width(grid.headers[j])
		for _, block := range blocks {
			for _, row := range block.rows {
				if w := lipgloss.Width(formatGridValue(cellAt(row, j, total))); w > grid.widths[j] {
					grid.widths[j] = w
				}
			}
		}
	}

	used, keep := indent+grid.boundWidth, 0
	for j := total - 1; j >= 0; j-- {
		if used+columnGap+grid.widths[j] > m.width {
			break
		}
		used += columnGap + grid.widths[j]
		keep++
	}
	if keep < 1 {
		// One overflowing line beats an empty grid on an absurdly narrow terminal.
		keep = 1
	}
	grid.headers = grid.headers[total-keep:]
	grid.widths = grid.widths[total-keep:]

	for _, block := range blocks {
		block.trimTo(total, keep)
	}
	return grid
}

// trimTo reduces every row to the keep newest of total columns and records the
// largest value left, which is the reference the block's shading is scaled to.
func (b *distBlock) trimTo(total, keep int) {
	b.max = 0
	for i, row := range b.rows {
		trimmed := make([]float64, keep)
		for j := range trimmed {
			trimmed[j] = cellAt(row, total-keep+j, total)
			if !math.IsNaN(trimmed[j]) && trimmed[j] > b.max {
				b.max = trimmed[j]
			}
		}
		b.rows[i] = trimmed
	}
}

// cellAt reads a transformed row right-aligned into a grid of n columns, so the
// family's newest value always lands in the last one.
func cellAt(row []float64, col, n int) float64 {
	pos := len(row) - 1 - (n - 1 - col)
	if pos < 0 || pos >= len(row) {
		return math.NaN()
	}
	return row[pos]
}

// renderBlock draws one expanded family's grid.
//
// Only histograms are shaded. A bucket value counts observations, but a summary's
// value is a latency: on one intensity scale a p99 of 0.09 would outshine a p50
// of 0.02 purely for being the larger number, reading as "more observations here"
// when it means nothing of the kind.
func (m model) renderBlock(block *distBlock, indent string, grid gridLayout) []string {
	if len(block.bounds) == 0 {
		return []string{indent + faintStyle.Render(nativeHistogramNote)}
	}

	header := make([]gridCell, len(grid.headers))
	for j, head := range grid.headers {
		header[j] = gridCell{text: head}
	}
	lines := []string{faintStyle.Render(renderGridRow(indent, block.label, grid.boundWidth, header, grid.widths))}

	shade := block.dist.Kind == KindHistogram
	for i, bound := range block.bounds {
		cells := make([]gridCell, len(grid.widths))
		for j := range cells {
			val := block.rows[i][j]
			cells[j] = gridCell{text: formatGridValue(val)}
			if shade {
				cells[j].style = shadeFor(val, block.max)
			}
		}
		lines = append(lines, renderGridRow(indent, bound, grid.boundWidth, cells, grid.widths))
	}
	return lines
}

// shadeFor picks the ramp step val earns against the block's own maximum, so
// distribution shape reads across a row and traffic volume reads down a column.
//
// Only positive values shade: an empty band should recede into the background
// rather than claim the darkest step, and with the delta key on a value can be
// negative, which no intensity scale can express.
func shadeFor(val, max float64) *lipgloss.Style {
	if max <= 0 || math.IsNaN(val) || val <= 0 {
		return nil
	}
	step := int(math.Ceil(val / max * float64(len(shadeRamp))))
	if step < 1 {
		step = 1
	}
	if step > len(shadeRamp) {
		step = len(shadeRamp)
	}
	return &shadeRamp[step-1]
}

// gridCell is one value in an expanded block together with the shade it earned.
type gridCell struct {
	text  string
	style *lipgloss.Style
}

// renderGridRow lays out one block row against the shared layout. A shaded cell
// carries its background across the gap to its left as well, so a row of busy
// buckets reads as a single band rather than as separated chips.
func renderGridRow(indent, label string, labelWidth int, cells []gridCell, widths []int) string {
	var sb strings.Builder
	sb.WriteString(indent)
	sb.WriteString(padCell(label, labelWidth, false))
	for i, width := range widths {
		chunk := strings.Repeat(" ", columnGap) + padCell(cells[i].text, width, true)
		if cells[i].style != nil {
			chunk = cells[i].style.Render(chunk)
		}
		sb.WriteString(chunk)
	}
	return sb.String()
}

// formatGridValue renders one grid value. A scrape the family did not report
// reads as a gap, never as a count of zero.
func formatGridValue(val float64) string {
	if math.IsNaN(val) {
		return "."
	}
	return formatFloat(val)
}

// historyColumns is how many scrapes the grid can show at most, which is however
// many the store was told to retain.
func (m model) historyColumns() int {
	if m.cfg.History < 1 {
		return 1
	}
	return m.cfg.History
}

// distributionHeader is the view's own title line. It carries the bucket mode
// because that setting only means anything here, and the footer is already full.
// It also carries whatever the aggregation field did not do here, which is the
// one thing about this view the rows cannot show by themselves.
func (m model) distributionHeader(entries []distEntry) string {
	title := lipgloss.NewStyle().Bold(true).Render("DISTRIBUTIONS")
	text := fmt.Sprintf("%d shown | Buckets: %s (b) | enter/esc: expand",
		len(entries), m.bucketMode)
	if note := m.distAggNote(entries); note != "" {
		text += " | " + note
	}
	return title + "  " + faintStyle.Render(text)
}

// collapsedCells builds one distribution's summary line as plain text. Styling is
// applied after widths are known, so a styled cell never throws the layout off.
func (m model) collapsedCells(entry distEntry) []string {
	dist := entry.dist
	total := distScrapeCount(dist)
	stats := distSummary(dist, total-1, m.cfg.Interval)

	count, rate := ".", "."
	if stats.CountOK {
		count = formatCompact(stats.Count)
	}
	if stats.RateOK {
		rate = formatCompact(stats.Rate) + "/s"
	}

	cells := []string{m.collapsedName(entry), count, rate}
	for _, cell := range stats.Cells {
		cells = append(cells, formatQuantile(cell))
	}
	return cells
}

// collapsedName is the family name with its labels, prefixed by the expansion
// marker. Kept plain; see collapsedCells.
//
// The marker reads the entry's own signature rather than deriving one from the
// family, because an aggregated row's name is decorated and its labels are
// narrowed: a signature rebuilt from those would never match the key the
// accordion recorded the expansion under.
func (m model) collapsedName(entry distEntry) string {
	marker := "▸"
	if m.expanded[entry.sig] {
		marker = "▾"
	}
	dist := entry.dist
	name := dist.Name
	if labels := m.formatDistLabels(dist); labels != "" {
		name += labels
	}
	return marker + " " + name
}

// formatDistLabels renders a family's labels under the current label mode. The
// family's own labels never include le or quantile, so the whole set belongs to
// the row.
func (m model) formatDistLabels(dist *DistributionSeries) string {
	if m.cfg.LabelMode == LabelModeHideAll || len(dist.Labels) == 0 {
		return ""
	}

	hidden := make(map[string]bool)
	if m.cfg.LabelMode == LabelModeHideFiltered {
		for _, key := range getFilteredLabelKeys(m.cfg.FilterLabel) {
			hidden[key] = true
		}
	}

	var parts []string
	for k, v := range dist.Labels {
		if hidden[k] {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return fmt.Sprintf("{%s}", strings.Join(parts, ","))
}

// formatQuantile renders one quantile cell. A rank landing in the +Inf bucket has
// no upper bound to interpolate towards, so it reads as "greater than the last
// finite bound" rather than as a number the bucket layout cannot support.
func formatQuantile(cell quantileCell) string {
	if !cell.OK {
		return "."
	}
	if cell.Beyond {
		return ">" + formatFloat(cell.Value)
	}
	return formatFloat(cell.Value)
}

// fitNameColumn shrinks the name column until the whole line fits the terminal.
// The value columns are already as narrow as their contents allow, so the name is
// the only one with slack to give up.
func (m model) fitNameColumn(widths []int) int {
	used := 0
	for i, w := range widths {
		if i > 0 {
			used += w + columnGap
		}
	}
	available := m.width - used
	// Enough to still tell two rows apart; below this the terminal is too narrow
	// for any layout and horizontal truncation is the lesser evil.
	const minNameWidth = 12
	if available < minNameWidth {
		available = minNameWidth
	}

	if widths[0] > available {
		return available
	}
	return widths[0]
}

// renderRow pads cells to the shared column layout: the leading name column left
// aligned, every value column right aligned under its header.
func renderRow(cells []string, widths []int) string {
	var sb strings.Builder
	for i, width := range widths {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		if i > 0 {
			sb.WriteString(strings.Repeat(" ", columnGap))
		}
		sb.WriteString(padCell(cell, width, i > 0))
	}
	return sb.String()
}

// padCell pads or truncates cell to width, measuring with lipgloss so an already
// styled cell is not mistaken for a wider one.
func padCell(cell string, width int, right bool) string {
	w := lipgloss.Width(cell)
	if w > width {
		return truncateMessage(cell, width)
	}
	pad := strings.Repeat(" ", width-w)
	if right {
		return pad + cell
	}
	return cell + pad
}

// emptyDistributionsMessage explains an empty list, distinguishing an exporter
// that publishes no histograms from filters that hid the ones it does.
func (m model) emptyDistributionsMessage() string {
	if len(m.store.Distributions) == 0 {
		return "No histograms or summaries to display"
	}
	var reasons []string
	if m.cfg.FilterMetric != "" {
		reasons = append(reasons, "metric filter")
	}
	if m.cfg.FilterLabel != "" {
		reasons = append(reasons, "label filter")
	}
	if m.cfg.HideStatic {
		reasons = append(reasons, "hide-static")
	}
	if len(reasons) == 0 {
		return "No histograms or summaries to display"
	}
	return fmt.Sprintf("No histograms or summaries to display (all %d hidden by: %s)",
		len(m.store.Distributions), strings.Join(reasons, ", "))
}
