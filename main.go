package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	dto "github.com/prometheus/client_model/go"
)

// Delta mode constants
const (
	DeltaModeOff  = "off"
	DeltaModeNext = "next"
	DeltaModeView = "view"
)

// Label mode constants
const (
	LabelModeShowAll      = "all"
	LabelModeHideFiltered = "hide-filtered"
	LabelModeHideAll      = "hide-all"
)

// Config holds the command line arguments
type Config struct {
	URL          string
	Interval     time.Duration
	History      int
	LabelMode    string
	FilterMetric string
	FilterLabel  string
	DeltaMode    string
	HideStatic   bool
	Aggregation  string
}

type model struct {
	cfg                 Config
	store               *Store
	fetcher             *Fetcher
	err                 error
	connectionError     error
	isConnected         bool
	lastSuccessfulFetch time.Time
	showHelp            bool
	isPaused            bool
	width               int
	height              int
	viewport            viewport.Model
	viewportReady       bool
	metricNameStyle     lipgloss.Style
	labelStyle          lipgloss.Style
	currentValueStyle   lipgloss.Style
	deltaValueStyle     lipgloss.Style
	cursorStyle         lipgloss.Style

	// view is the top-level view on screen; the two remember their scroll
	// position independently so switching back and forth is a round trip.
	view           ViewMode
	metricsYOffset int
	distYOffset    int

	// bucketMode, distCursor, expanded and zoomed belong to the distribution
	// view. expanded and zoomed are keyed by store signature, so the accordion
	// survives a scrape that reorders nothing but could otherwise invalidate an
	// index. zoomed is empty when no family has the screen to itself.
	bucketMode     BucketMode
	distCursor     int
	distCursorSpan cursorSpan
	expanded       map[string]bool
	zoomed         string

	// editing names the header box that currently owns the keyboard, and input
	// is the one text field shared by all three boxes - only one can be focused
	// at a time, so a field per box would be three copies of the same state.
	// inputErr holds the reason the last commit was refused, which keeps the
	// user in the box instead of applying a pattern that matches nothing.
	editing  headerField
	input    textinput.Model
	inputErr string
}

type tickMsg time.Time

func main() {
	cfg := parseFlags()

	if cfg.URL == "" {
		fmt.Println("Error: -url argument is required")
		flag.Usage()
		os.Exit(1)
	}

	// The same validation the header boxes apply to an edited filter, so that a
	// flag and a keystroke cannot disagree about what is a usable filter.
	if err := validateMetricFilter(cfg.FilterMetric); err != nil {
		fmt.Printf("Error: invalid metric filter: %v\n", err)
		os.Exit(1)
	}
	if err := validateLabelFilter(cfg.FilterLabel); err != nil {
		fmt.Printf("Error: invalid label filter: %v\n", err)
		os.Exit(1)
	}
	if err := validateAggregation(cfg.Aggregation); err != nil {
		fmt.Printf("Error: invalid aggregation: %v\n", err)
		os.Exit(1)
	}

	store := NewStore(cfg.History)
	fetcher := NewFetcher(cfg.URL)

	metricNameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	labelStyle := lipgloss.NewStyle().Faint(true)
	currentValueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("213")) // brighter magenta
	deltaValueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("208"))   // orange
	cursorStyle := lipgloss.NewStyle().Background(lipgloss.Color("238"))

	// A static cursor keeps the header boxes out of the message loop: a blinking
	// one would need cursor.BlinkMsg plumbed through Update alongside the tick
	// and fetch messages, for no gain in a bar that is only ever briefly focused.
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 256
	input.Cursor.SetMode(cursor.CursorStatic)

	m := model{
		cfg:               cfg,
		store:             store,
		fetcher:           fetcher,
		width:             80,
		height:            24,
		metricNameStyle:   metricNameStyle,
		labelStyle:        labelStyle,
		currentValueStyle: currentValueStyle,
		deltaValueStyle:   deltaValueStyle,
		cursorStyle:       cursorStyle,
		// Raw cumulative counters are the least readable of the three bucket
		// modes, so open on the most readable one.
		bucketMode: BucketModePerBucketDelta,
		expanded:   make(map[string]bool),
		input:      input,
	}

	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Printf("Error running program: %v\n", err)
		os.Exit(1)
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchCmd(),
		m.tickCmd(),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// A focused header box swallows the keyboard, so that typing a filter
		// cannot trip over the single-letter global keys - q would otherwise quit
		// halfway through a metric name.
		if m.editing != fieldNone {
			switch msg.String() {
			case "enter":
				m.commitEditing()
				return m, nil
			case "esc":
				m.cancelEditing()
				return m, nil
			case "tab":
				return m, m.moveField(1)
			case "shift+tab":
				return m, m.moveField(-1)
			case "ctrl+c":
				return m, tea.Quit
			}
			// A new keystroke means the user is fixing what the last commit
			// refused, so the complaint goes away until they try again.
			m.inputErr = ""
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = !m.showHelp
			return m, nil
		case "l":
			// Cycle through label modes. The "hide-filtered" mode is only offered
			// when the filter pins a label value down - a bare-regex filter names
			// no label, so the mode would hide nothing.
			if len(getFilteredLabelKeys(m.cfg.FilterLabel)) == 0 {
				// Simple toggle: all <-> hide-all
				if m.cfg.LabelMode == LabelModeShowAll {
					m.cfg.LabelMode = LabelModeHideAll
				} else {
					m.cfg.LabelMode = LabelModeShowAll
				}
			} else {
				// Full cycle: all -> hide-filtered -> hide-all -> all
				switch m.cfg.LabelMode {
				case LabelModeShowAll:
					m.cfg.LabelMode = LabelModeHideFiltered
				case LabelModeHideFiltered:
					m.cfg.LabelMode = LabelModeHideAll
				case LabelModeHideAll:
					m.cfg.LabelMode = LabelModeShowAll
				default:
					m.cfg.LabelMode = LabelModeShowAll
				}
			}
			m.refresh()
			return m, nil
		case "d":
			// Cycle through delta modes: off -> next -> view -> off
			switch m.cfg.DeltaMode {
			case DeltaModeOff:
				m.cfg.DeltaMode = DeltaModeNext
			case DeltaModeNext:
				m.cfg.DeltaMode = DeltaModeView
			case DeltaModeView:
				m.cfg.DeltaMode = DeltaModeOff
			default:
				m.cfg.DeltaMode = DeltaModeOff
			}
			m.refresh()
			return m, nil
		case "p":
			m.isPaused = !m.isPaused
			return m, nil
		case "s":
			m.cfg.HideStatic = !m.cfg.HideStatic
			m.refresh()
			return m, nil
		case "v":
			m.toggleView()
			return m, nil
		case "m":
			return m, m.startEditing(fieldMetricFilter)
		case "f":
			return m, m.startEditing(fieldLabelFilter)
		case "a":
			return m, m.startEditing(fieldAggregation)
		case "b":
			// Scoped to the distribution view: the bucket mode means nothing in the
			// metrics view, where b stays the viewport's page-up key.
			if m.view == ViewDistributions {
				m.bucketMode = m.bucketMode.next()
				m.refresh()
				return m, nil
			}
		case "enter":
			if m.view == ViewDistributions {
				m.expandStep()
				return m, nil
			}
		case "esc":
			if m.view == ViewDistributions {
				m.collapseStep()
				return m, nil
			}
		case "up", "down", "k", "j":
			// In the distribution view these move the cursor rather than the
			// viewport; refresh scrolls far enough to keep the cursor on screen.
			// A zoomed family is the only thing on screen, so there is nothing to
			// point at and the keys go back to scrolling its grid.
			if m.view == ViewDistributions && m.zoomed == "" {
				delta := -1
				if key := msg.String(); key == "down" || key == "j" {
					delta = 1
				}
				m.moveCursor(delta)
				return m, nil
			}
		}

		// Anything not handled above, plus the view-scoped keys in the view they do
		// not apply to, falls through to the viewport for scrolling.
		if m.viewportReady {
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
	case tickMsg:
		if m.isPaused {
			// When paused, only schedule next tick (no fetch)
			return m, m.tickCmd()
		}
		// When not paused, do both fetch and schedule next tick
		return m, tea.Batch(m.fetchCmd(), m.tickCmd())
	case map[string]*dto.MetricFamily: // Fetch result
		if m.isPaused {
			// Ignore fetch results while paused
			return m, nil
		}
		m.store.UpdateFromFamilies(msg)
		m.isConnected = true
		m.connectionError = nil
		m.lastSuccessfulFetch = time.Now()
		m.refresh()
		return m, nil
	case error:
		// Store connection error but keep retrying
		m.connectionError = msg
		m.isConnected = false
		// Don't set m.err - that's for fatal errors only
		// The tick/fetch cycle continues automatically
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		// Initialize or resize viewport
		// Reserve the header's rows plus 2 lines: 1 for footer, 1 safety margin
		viewportHeight := msg.Height - headerHeight - 2
		if viewportHeight < 1 {
			viewportHeight = 1
		}

		if !m.viewportReady {
			m.viewport = viewport.New(msg.Width, viewportHeight)
			m.viewport.MouseWheelEnabled = true
			m.viewportReady = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = viewportHeight
		}

		m.refresh()
	}

	return m, nil
}

// refresh rebuilds the viewport content for whichever view is on screen. Every
// key that changes what is displayed ends in a call to this.
func (m *model) refresh() {
	if !m.viewportReady {
		return
	}
	if m.view != ViewDistributions {
		m.viewport.SetContent(m.buildTable())
		return
	}
	m.clampDistState()
	content, span := m.renderDistributions()
	m.viewport.SetContent(content)
	if m.zoomed != "" {
		// A zoomed grid has no cursor to follow, and the user scrolls it by hand;
		// a scrape must not yank the view back to the top under them.
		return
	}
	m.distCursorSpan = span
	m.revealSpan(span)
}

// expandStep walks the accordion one rung up - collapsed, expanded, zoomed - so
// one key reaches every level of detail and stops at the top rather than cycling
// back to where the user came from.
func (m *model) expandStep() {
	entries := m.visibleDistributions()
	if m.distCursor < 0 || m.distCursor >= len(entries) {
		return
	}
	sig := entries[m.distCursor].sig
	switch {
	case !m.expanded[sig]:
		m.expanded[sig] = true
	case m.zoomed != sig:
		m.zoomed = sig
		if m.viewportReady {
			m.viewport.SetYOffset(0)
		}
	default:
		return
	}
	m.refresh()
}

// collapseStep walks back down the same ladder, one rung per press.
func (m *model) collapseStep() {
	if m.zoomed != "" {
		m.zoomed = ""
		if m.viewportReady {
			// The zoomed grid may have been scrolled far past the end of the list
			// that is about to replace it.
			m.viewport.SetYOffset(0)
		}
		m.refresh()
		return
	}
	entries := m.visibleDistributions()
	if m.distCursor < 0 || m.distCursor >= len(entries) {
		return
	}
	delete(m.expanded, entries[m.distCursor].sig)
	m.refresh()
}

// toggleView switches between the two views, remembering each one's scroll
// position so that pressing v twice is a round trip. The content is replaced
// before the offset is restored, because the viewport clamps an offset against
// whatever it currently holds - not against what is about to be put in it.
func (m *model) toggleView() {
	if !m.viewportReady {
		if m.view == ViewMetrics {
			m.view = ViewDistributions
		} else {
			m.view = ViewMetrics
		}
		return
	}

	if m.view == ViewMetrics {
		m.metricsYOffset = m.viewport.YOffset
		m.view = ViewDistributions
		m.viewport.SetYOffset(0)
		m.refresh()
		m.viewport.SetYOffset(m.distYOffset)
		if m.zoomed == "" {
			// The remembered offset may predate a shorter list, hiding the cursor.
			// A zoomed grid has no cursor, and distCursorSpan still refers to the
			// list it was last measured against, so honouring it would jump.
			m.revealSpan(m.distCursorSpan)
		}
		return
	}

	m.distYOffset = m.viewport.YOffset
	m.view = ViewMetrics
	m.viewport.SetYOffset(0)
	m.refresh()
	m.viewport.SetYOffset(m.metricsYOffset)
}

// moveCursor moves the distribution cursor by delta, clamping at both ends rather
// than wrapping so holding a key cannot silently jump to the far end of the list.
func (m *model) moveCursor(delta int) {
	m.distCursor += delta
	m.refresh()
}

// clampDistState keeps the cursor on a row that exists and drops a zoom whose
// family has gone, since filters, the hide-static toggle and the exporter itself
// all change the list out from under both.
func (m *model) clampDistState() {
	entries := m.visibleDistributions()
	if m.distCursor >= len(entries) {
		m.distCursor = len(entries) - 1
	}
	if m.distCursor < 0 {
		m.distCursor = 0
	}
	if m.zoomed == "" {
		return
	}
	for _, entry := range entries {
		if entry.sig == m.zoomed {
			return
		}
	}
	m.zoomed = ""
}

// revealSpan nudges the viewport just far enough to show the cursor's line and
// the grid expanded beneath it, leaving the offset untouched when the span is
// already on screen so a scrape does not jump the view.
//
// A block taller than the viewport cannot be shown whole, so it is anchored at
// the cursor line and the rest is left to the user's own scrolling. An offset
// that already sits inside such a span is one the user scrolled to deliberately
// and is left alone, rather than being dragged back to the top of the block on
// every scrape while they read further down it.
func (m *model) revealSpan(span cursorSpan) {
	height := m.viewport.Height
	top := m.viewport.YOffset
	bottom := top + height - 1

	switch {
	case span.start >= top && span.end <= bottom:
		return
	case span.end-span.start+1 > height:
		if top < span.start || top > span.end {
			m.viewport.SetYOffset(span.start)
		}
	case span.start < top:
		m.viewport.SetYOffset(span.start)
	default:
		m.viewport.SetYOffset(span.end - height + 1)
	}
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\n\nPress q to quit.", m.err)
	}

	if !m.viewportReady {
		return "Initializing..."
	}

	// Build status indicator (URL with connection status)
	connectedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("71")) // dimmer green
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))    // red
	scrollHintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Faint(true)

	// Build delta status first to measure it
	deltasStatus := "Off"
	switch m.cfg.DeltaMode {
	case DeltaModeNext:
		deltasStatus = m.deltaValueStyle.Render("Δ") + " Next"
	case DeltaModeView:
		deltasStatus = m.deltaValueStyle.Render("Δ") + " View"
	}

	// Build pause status
	var pauseStatus string
	if m.isPaused {
		pauseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
		pauseStatus = " | " + pauseStyle.Render("⏸  PAUSED")
	}

	// Build the view indicator. It names the key as well as the view, because it
	// is the only place the second view is advertised outside the help overlay.
	viewName := "Metrics"
	if m.view == ViewDistributions {
		viewName = "Distributions"
	}
	viewStatus := lipgloss.NewStyle().Foreground(lipgloss.Color("111")).Render("v: " + viewName)

	// Build hide-static status
	var hideStaticStatus string
	if m.cfg.HideStatic {
		hideStaticStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
		hideStaticStatus = " | " + hideStaticStyle.Render("Static: Hidden")
	}

	// Build scroll hints
	var scrollHints string
	if !m.viewport.AtTop() && !m.viewport.AtBottom() {
		scrollHints = scrollHintStyle.Render(" ▲▼")
	} else if !m.viewport.AtTop() {
		scrollHints = scrollHintStyle.Render(" ▲")
	} else if !m.viewport.AtBottom() {
		scrollHints = scrollHintStyle.Render(" ▼")
	}

	safetyMargin := 3
	fixedSeparator := " | "

	// The footer has one line and, while a header box is open, two things that
	// want it: the edit hint and the endpoint status. The mode indicators step
	// aside for the duration - they are static information, still there the
	// moment the user presses esc - because a regex error is as long as the regex
	// and would otherwise wrap the footer, costing a row of the table.
	editing := m.editing != fieldNone

	// Everything in the footer except its two variable-width parts: the left
	// segment and the status message.
	fixedWidth := lipgloss.Width(fixedSeparator) +
		lipgloss.Width(scrollHints) +
		lipgloss.Width("● ") // Approximate icon width
	if !editing {
		fixedWidth += lipgloss.Width(" |  | Deltas: ") +
			lipgloss.Width(viewStatus) +
			lipgloss.Width(deltasStatus) +
			lipgloss.Width(pauseStatus) +
			lipgloss.Width(hideStaticStatus)
	}

	// The footer's left segment doubles as the edit hint, because the header is a
	// fixed height with no room for one.
	leftSegment := "? for help"
	if editing {
		leftSegment = m.headerHint(m.width - fixedWidth - safetyMargin - statusReserveWhileEdit)
	}

	// Whatever the rest of the line does not want. This used to be floored at a
	// readable minimum, which pushed the footer past the terminal's right edge on
	// a narrow window - and a wrapped footer costs a row of the table, which is
	// worse than a short URL.
	maxMessageLength := max(m.width-fixedWidth-lipgloss.Width(leftSegment)-safetyMargin, 0)

	// Build status indicator with dynamic truncation
	var statusIndicator string
	if m.isConnected {
		// Connected - show URL with truncation
		url := truncateMessage(m.cfg.URL, maxMessageLength)
		statusIndicator = connectedStyle.Render("● ") + url
	} else if m.connectionError != nil {
		// Error - show error message with truncation
		errMsg := truncateMessage(m.connectionError.Error(), maxMessageLength)
		statusIndicator = errorStyle.Render("⚠ " + errMsg)
	} else {
		// Initial connecting state - show URL with truncation
		url := truncateMessage(m.cfg.URL, maxMessageLength)
		statusIndicator = lipgloss.NewStyle().Faint(true).Render("● ") + url
	}

	footer := fmt.Sprintf("%s | %s | Deltas: %s%s%s | %s%s",
		leftSegment, viewStatus, deltasStatus, pauseStatus, hideStaticStatus, statusIndicator, scrollHints)
	if editing {
		footer = fmt.Sprintf("%s | %s%s", leftSegment, statusIndicator, scrollHints)
	}

	// Show help popup if toggled
	output := m.renderHeader() + "\n" + m.viewport.View() + "\n" + footer
	if m.showHelp {
		output = m.renderHelpOverlay(output)
	}

	return output
}

// truncateMessage truncates a message to maxLen, adding "..." if truncated
func truncateMessage(msg string, maxLen int) string {
	if maxLen < 4 {
		maxLen = 4 // Minimum to fit "..."
	}
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen-3] + "..."
}

func (m model) renderHelpOverlay(content string) string {
	helpText := `
Help

  q/ctrl+c    Quit
  ?           Toggle this help
  l           Cycle label display mode
  d           Cycle delta mode (off/next/view)
  p           Pause/unpause updates
  s           Toggle hiding static (unchanging) metrics
  v           Switch between metrics and distributions
  m           Edit the metric-name filter
  f           Edit the label filter (k=v,k!=v,k=~re - all must match)
  a           Edit the aggregation (labels to group by, e.g. pod or avg pod)
  enter/esc   Apply / discard a header edit
  tab         Move to the next header field
  b           Cycle bucket values (distribution view)
  enter       Expand a distribution, then zoom it full screen
  esc         Step back down: zoomed -> expanded -> collapsed
  ↑/↓         Scroll, or move the cursor in the distribution view
  PgUp/PgDn   Page up/down (f is the filter key, not page-down)
  Home/End    Go to top/bottom

Press ? to close
`

	// Create a styled box for the help
	helpStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63")).
		Padding(1, 2).
		Background(lipgloss.Color("235")).
		Foreground(lipgloss.Color("252"))

	helpBox := helpStyle.Render(helpText)

	// Overlay the help on top of content using Place
	return lipgloss.Place(
		m.width,
		m.height,
		lipgloss.Center,
		lipgloss.Center,
		helpBox,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("0")),
	)
}

var baseStyle = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderForeground(lipgloss.Color("240"))

func (m model) tickCmd() tea.Cmd {
	return tea.Tick(m.cfg.Interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) fetchCmd() tea.Cmd {
	return func() tea.Msg {
		families, err := m.fetcher.Fetch()
		if err != nil {
			return err
		}
		return families
	}
}

func formatMetricName(series *MetricSeries, hideLabels bool) string {
	name := series.Name
	if !hideLabels && len(series.Labels) > 0 {
		var labelParts []string
		for k, v := range series.Labels {
			labelParts = append(labelParts, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(labelParts)
		name += fmt.Sprintf("{%s}", strings.Join(labelParts, ","))
	}
	return name
}

func calculateColumnWidths(headers []string, rows [][]string) []int {
	if len(rows) == 0 && len(headers) == 0 {
		return []int{}
	}

	// Find the max number of columns (consider both headers and rows)
	maxCols := len(headers)
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}

	// Calculate max width for each column (content only, no padding)
	widths := make([]int, maxCols)
	for colIdx := 0; colIdx < maxCols; colIdx++ {
		maxWidth := 0

		// Check header width
		if colIdx < len(headers) {
			headerWidth := lipgloss.Width(headers[colIdx])
			if headerWidth > maxWidth {
				maxWidth = headerWidth
			}
		}

		// Check data cell widths
		for _, row := range rows {
			if colIdx < len(row) {
				cellWidth := lipgloss.Width(row[colIdx])
				if cellWidth > maxWidth {
					maxWidth = cellWidth
				}
			}
		}
		widths[colIdx] = maxWidth
	}

	return widths
}

func (m model) buildTableRows(filteredSeries []*MetricSeries) [][]string {
	rows := [][]string{}
	for _, series := range filteredSeries {
		// Style metric name and labels based on label mode
		styledName := m.metricNameStyle.Render(series.Name)

		// Determine which labels to show based on mode
		if m.cfg.LabelMode != LabelModeHideAll && len(series.Labels) > 0 {
			var labelParts []string

			if m.cfg.LabelMode == LabelModeHideFiltered {
				// Hide only the filtered label keys
				filteredKeys := getFilteredLabelKeys(m.cfg.FilterLabel)
				filteredKeyMap := make(map[string]bool)
				for _, key := range filteredKeys {
					filteredKeyMap[key] = true
				}

				// Only include labels whose keys are NOT in the filter
				for k, v := range series.Labels {
					if !filteredKeyMap[k] {
						labelParts = append(labelParts, fmt.Sprintf("%s=%s", k, v))
					}
				}
			} else {
				// LabelModeShowAll - show all labels
				for k, v := range series.Labels {
					labelParts = append(labelParts, fmt.Sprintf("%s=%s", k, v))
				}
			}

			if len(labelParts) > 0 {
				sort.Strings(labelParts)
				styledName = styledName + m.labelStyle.Render(fmt.Sprintf("{%s}", strings.Join(labelParts, ",")))
			}
		}

		row := []string{styledName}

		// Get values - build all possible value columns up to history limit
		vals := series.ValuesWithDeltas(m.cfg.DeltaMode)
		numValueCols := m.cfg.History
		if numValueCols < 1 {
			numValueCols = 1
		}

		// Create value columns
		for i := 0; i < numValueCols; i++ {
			offset := numValueCols - 1 - i
			valIdx := len(vals) - 1 - offset
			isCurrentValue := (i == numValueCols-1)

			if valIdx >= 0 && valIdx < len(vals) {
				val := vals[valIdx]
				if math.IsNaN(val) {
					row = append(row, ".")
				} else {
					formatted := formatFloat(val)
					isDeltaValue := false

					// Determine if this should be displayed as a delta value
					switch m.cfg.DeltaMode {
					case DeltaModeNext:
						// In 'next' mode, all historical values are deltas, current is absolute
						isDeltaValue = !isCurrentValue
					case DeltaModeView:
						// In 'view' mode, all values including current are deltas
						isDeltaValue = true
					}

					if isDeltaValue {
						// Delta values
						if formatted == "0" || formatted == "-0" {
							formatted = "."
						} else {
							// Add explicit sign for deltas
							if val > 0 {
								formatted = "+" + formatted
							}
							formatted = m.deltaValueStyle.Render(formatted)
						}
					} else if isCurrentValue {
						// Current value in non-delta modes is shown in magenta
						formatted = m.currentValueStyle.Render(formatted)
					}
					row = append(row, formatted)
				}
			} else {
				row = append(row, "")
			}
		}

		rows = append(rows, row)
	}
	return rows
}

// distributionHint points at the other view when this one has nothing to show
// but histograms were scraped - otherwise an exporter that publishes only
// histograms looks like an exporter that publishes nothing at all.
func (m model) distributionHint() string {
	count := len(m.store.Distributions)
	if count == 0 {
		return ""
	}
	noun := "distributions"
	if count == 1 {
		noun = "distribution"
	}
	return fmt.Sprintf("\n\n%d %s scraped - press v to see them", count, noun)
}

func (m model) buildTable() string {
	// Filter metrics first
	var filteredSeries []*MetricSeries
	keys := make([]string, 0, len(m.store.Metrics))
	for k := range m.store.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		series := m.store.Metrics[k]
		if !m.matchesFilters(series.Name, series.Labels) {
			continue
		}
		filteredSeries = append(filteredSeries, series)
	}

	// Filter, then aggregate, then hide static - in that order. Aggregating first
	// would fold in series the filter was asked to exclude, and judging staticness
	// first would drop a member whose group is not static: the sum over a fixed
	// series and a moving one moves, and is worth showing.
	filteredSeries = m.aggregateSeries(filteredSeries)

	if m.cfg.HideStatic {
		kept := filteredSeries[:0]
		for _, series := range filteredSeries {
			if series.IsStatic() {
				continue
			}
			kept = append(kept, series)
		}
		filteredSeries = kept
	}

	if len(filteredSeries) == 0 {
		if len(m.store.Metrics) == 0 {
			return "No metrics to display" + m.distributionHint()
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
			return "No metrics to display" + m.distributionHint()
		}
		return fmt.Sprintf("No metrics to display (all %d metrics hidden by: %s)%s",
			len(m.store.Metrics), strings.Join(reasons, ", "), m.distributionHint())
	}

	// Build rows with all possible columns first
	allRows := m.buildTableRows(filteredSeries)

	// Build headers for all possible columns
	maxPossibleValueCols := m.cfg.History
	if maxPossibleValueCols < 1 {
		maxPossibleValueCols = 1
	}
	allHeaders := []string{"Metric"}
	for i := 0; i < maxPossibleValueCols; i++ {
		title := fmt.Sprintf("-%ds", (maxPossibleValueCols-1-i)*int(m.cfg.Interval.Seconds()))
		if i == maxPossibleValueCols-1 {
			title = "Curr"
		}
		allHeaders = append(allHeaders, title)
	}

	// Calculate column widths from headers and data
	colWidths := calculateColumnWidths(allHeaders, allRows)

	// Calculate how many value columns will fit in terminal width
	// Table width formula: sum(column_widths) + (num_columns + 1) for borders
	usedWidth := 1 // Start with left border
	if len(colWidths) > 0 {
		usedWidth += colWidths[0] + 1 // metric name column + its right border
	}

	numValueCols := 0
	maxPossibleCols := len(colWidths) - 1 // Subtract 1 for metric name column

	// Add value columns from right to left (current going back in time)
	// Column indices: [0] = metric name, [1..N] = value columns (oldest to newest)
	for i := 0; i < maxPossibleCols; i++ {
		colIdx := len(colWidths) - 1 - i // Start from rightmost (newest) column
		if colIdx > 0 && colIdx < len(colWidths) {
			// Each additional column adds: column_width + 1 border
			if usedWidth+colWidths[colIdx]+1 <= m.width {
				usedWidth += colWidths[colIdx] + 1
				numValueCols++
			} else {
				break
			}
		}
	}

	if numValueCols < 1 {
		numValueCols = 1
	}

	// Trim rows to fit the calculated number of columns
	rows := make([][]string, len(allRows))
	for i, row := range allRows {
		// Keep metric name column + numValueCols from the end
		trimmedRow := []string{row[0]}
		startCol := len(row) - numValueCols
		if startCol < 1 {
			startCol = 1
		}
		trimmedRow = append(trimmedRow, row[startCol:]...)
		rows[i] = trimmedRow
	}

	// Trim headers to match the number of columns we're showing
	headers := []string{allHeaders[0]} // Keep "Metric"
	startHeaderCol := len(allHeaders) - numValueCols
	if startHeaderCol < 1 {
		startHeaderCol = 1
	}
	headers = append(headers, allHeaders[startHeaderCol:]...)

	// Create table
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("240"))).
		Headers(headers...).
		Rows(rows...)

	return t.Render()
}

func parseFlags() Config {
	var cfg Config
	flag.StringVar(&cfg.URL, "url", "", "URL to poll metrics from (required)")
	flag.DurationVar(&cfg.Interval, "interval", 5*time.Second, "Polling interval")
	flag.IntVar(&cfg.History, "history", 10, "Number of historical samples to keep")
	flag.StringVar(&cfg.LabelMode, "label-mode", LabelModeShowAll, "Label display mode: all, hide-filtered, hide-all")
	flag.StringVar(&cfg.FilterMetric, "filter-metric", "", "Regex to filter metrics by name")
	flag.StringVar(&cfg.FilterLabel, "filter-label", "", "Label filter: comma-separated clauses that must all match, each key=value, key!=value, key=~regex, key!~regex, or a bare regex tried against every label value (e.g. 'env=prod,region!=eu')")
	flag.StringVar(&cfg.DeltaMode, "delta-mode", DeltaModeOff, "Delta mode: off, next, view")
	flag.BoolVar(&cfg.HideStatic, "hide-static", false, "Hide metrics whose recent values never change")
	flag.StringVar(&cfg.Aggregation, "aggregation", "", "Combine series that differ only in other labels: a comma-separated label list, optionally preceded by sum, avg, min, max or count (e.g. 'pod', 'avg pod,instance', 'count')")

	flag.Parse()

	// Validate label mode
	switch cfg.LabelMode {
	case LabelModeShowAll, LabelModeHideFiltered, LabelModeHideAll:
		// Valid mode
	default:
		fmt.Printf("Error: invalid label mode '%s'. Must be one of: all, hide-filtered, hide-all\n", cfg.LabelMode)
		os.Exit(1)
	}

	// Validate delta mode
	switch cfg.DeltaMode {
	case DeltaModeOff, DeltaModeNext, DeltaModeView:
		// Valid mode
	default:
		fmt.Printf("Error: invalid delta mode '%s'. Must be one of: off, next, view\n", cfg.DeltaMode)
		os.Exit(1)
	}

	return cfg
}

// formatFloat renders a value for a grid cell, exactly enough that small
// numbers stay distinguishable from zero. Integers are exact; fractions carry
// roughly three significant digits without ever dropping an integer digit, so
// 0.0034 stays 0.0034 and 21203 stays 21203.
//
// The previous "%.2f" rounded any latency below 10ms to "0", making a healthy
// p99 indistinguishable from no observations at all.
func formatFloat(val float64) string {
	if math.IsNaN(val) {
		return "NaN"
	}
	abs := math.Abs(val)
	if math.IsInf(val, 0) || abs >= 1e15 {
		// Beyond float64's exact integer range "%f" spells out meaningless
		// digits; fall back to the exponent form.
		return strconv.FormatFloat(val, 'g', 3, 64)
	}
	if val == math.Trunc(val) {
		return strconv.FormatFloat(val, 'f', -1, 64)
	}

	// Decimal places chosen so the result carries ~3 significant digits. Below 1
	// the leading zeros are not significant, so let 'g' count digits instead.
	var s string
	switch {
	case abs >= 100:
		s = strconv.FormatFloat(val, 'f', 0, 64)
	case abs >= 10:
		s = strconv.FormatFloat(val, 'f', 1, 64)
	case abs >= 1:
		s = strconv.FormatFloat(val, 'f', 2, 64)
	default:
		s = strconv.FormatFloat(val, 'g', 3, 64)
	}
	return trimTrailingZeros(s)
}

// formatCompact renders a value with an SI suffix. Used only where width is at a
// premium - the collapsed distribution line's count and rate columns - never in
// a grid cell, where an exact value matters more than a short one.
func formatCompact(val float64) string {
	abs := math.Abs(val)
	if math.IsNaN(val) || math.IsInf(val, 0) || abs < 1000 {
		return formatFloat(val)
	}

	for _, unit := range []struct {
		limit  float64
		suffix string
	}{{1e12, "T"}, {1e9, "G"}, {1e6, "M"}, {1e3, "k"}} {
		if abs < unit.limit {
			continue
		}
		scaled := val / unit.limit
		// One decimal below 100 (12.4k), none above (124k) - three digits either way.
		decimals := 0
		if math.Abs(scaled) < 100 {
			decimals = 1
		}
		return trimTrailingZeros(strconv.FormatFloat(scaled, 'f', decimals, 64)) + unit.suffix
	}
	return formatFloat(val)
}

// trimTrailingZeros drops the padding "%f" adds, but only from a fractional
// part - "1200" must not become "12".
func trimTrailingZeros(s string) string {
	if !strings.Contains(s, ".") || strings.ContainsAny(s, "eE") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
