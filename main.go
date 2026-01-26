package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

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
}

type tickMsg time.Time

func main() {
	cfg := parseFlags()

	if cfg.URL == "" {
		fmt.Println("Error: -url argument is required")
		flag.Usage()
		os.Exit(1)
	}

	// Validate regex
	if _, err := regexp.Compile(cfg.FilterMetric); err != nil {
		fmt.Printf("Error: invalid metric filter regex: %v\n", err)
		os.Exit(1)
	}
	if _, err := regexp.Compile(cfg.FilterLabel); err != nil {
		fmt.Printf("Error: invalid label filter regex: %v\n", err)
		os.Exit(1)
	}

	store := NewStore(cfg.History)
	fetcher := NewFetcher(cfg.URL)

	metricNameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	labelStyle := lipgloss.NewStyle().Faint(true)
	currentValueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("213")) // brighter magenta
	deltaValueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("208"))   // orange

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
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = !m.showHelp
			return m, nil
		case "l":
			// Cycle through label modes
			// If FilterLabel is empty, skip the "hide-filtered" mode
			if m.cfg.FilterLabel == "" {
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
			// Update viewport content when label mode changes
			if m.viewportReady {
				tableStr := m.buildTable()
				m.viewport.SetContent(tableStr)
			}
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
			// Update viewport content when delta mode changes
			if m.viewportReady {
				tableStr := m.buildTable()
				m.viewport.SetContent(tableStr)
			}
			return m, nil
		case "p":
			m.isPaused = !m.isPaused
			return m, nil
		default:
			// Delegate other keys to viewport for scrolling
			if m.viewportReady {
				m.viewport, cmd = m.viewport.Update(msg)
				return m, cmd
			}
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
		// Update viewport content with new data
		if m.viewportReady {
			tableStr := m.buildTable()
			m.viewport.SetContent(tableStr)
		}
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
		// Reserve 2 lines: 1 for footer, 1 for safety margin
		viewportHeight := msg.Height - 2
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

		// Update viewport content with current table
		if m.viewportReady {
			tableStr := m.buildTable()
			m.viewport.SetContent(tableStr)
		}
	}

	return m, nil
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

	// Build scroll hints
	var scrollHints string
	if !m.viewport.AtTop() && !m.viewport.AtBottom() {
		scrollHints = scrollHintStyle.Render(" ▲▼")
	} else if !m.viewport.AtTop() {
		scrollHints = scrollHintStyle.Render(" ▲")
	} else if !m.viewport.AtBottom() {
		scrollHints = scrollHintStyle.Render(" ▼")
	}

	// Calculate available space for error/URL message
	fixedPrefix := "? for help | Deltas: "
	fixedSeparator := " | "
	fixedWidth := lipgloss.Width(fixedPrefix) +
		lipgloss.Width(deltasStatus) +
		lipgloss.Width(pauseStatus) +
		lipgloss.Width(fixedSeparator) +
		lipgloss.Width(scrollHints) +
		lipgloss.Width("● ") // Approximate icon width

	safetyMargin := 3
	maxMessageLength := m.width - fixedWidth - safetyMargin
	if maxMessageLength < 20 {
		maxMessageLength = 20
	}

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

	footer := fmt.Sprintf("? for help | Deltas: %s%s | %s%s", deltasStatus, pauseStatus, statusIndicator, scrollHints)

	// Show help popup if toggled
	output := m.viewport.View() + "\n" + footer
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
  ↑/↓         Scroll up/down
  PgUp/PgDn   Page up/down
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

// getFilteredLabelKeys extracts the label key(s) from a filter pattern
// Returns the label keys that are being filtered on
func getFilteredLabelKeys(filterLabel string) []string {
	if filterLabel == "" {
		return []string{}
	}

	// Check for key=value or key=~value pattern
	if idx := strings.Index(filterLabel, "="); idx != -1 {
		key := filterLabel[:idx]
		return []string{key}
	}

	// Fallback regex pattern - can't determine specific keys
	return []string{}
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
		// Apply filters
		if m.cfg.FilterMetric != "" {
			matched, _ := regexp.MatchString(m.cfg.FilterMetric, series.Name)
			if !matched {
				continue
			}
		}
		if m.cfg.FilterLabel != "" {
			matched := false

			// Check for key=value or key=~value
			if idx := strings.Index(m.cfg.FilterLabel, "="); idx != -1 {
				key := m.cfg.FilterLabel[:idx]
				rest := m.cfg.FilterLabel[idx+1:]

				// Check if it is a regex match (starts with ~)
				if strings.HasPrefix(rest, "~") {
					pattern := rest[1:]
					if val, ok := series.Labels[key]; ok {
						if ok, _ := regexp.MatchString(pattern, val); ok {
							matched = true
						}
					}
				} else {
					// Exact match
					if val, ok := series.Labels[key]; ok {
						if val == rest {
							matched = true
						}
					}
				}
			} else {
				// Fallback: match value against regex (original behavior)
				for _, v := range series.Labels {
					if ok, _ := regexp.MatchString(m.cfg.FilterLabel, v); ok {
						matched = true
						break
					}
				}
			}

			if !matched {
				continue
			}
		}
		filteredSeries = append(filteredSeries, series)
	}

	if len(filteredSeries) == 0 {
		return "No metrics to display"
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
	flag.StringVar(&cfg.FilterLabel, "filter-label", "", "Regex to filter metrics by label (e.g. 'env=prod')")
	flag.StringVar(&cfg.DeltaMode, "delta-mode", DeltaModeOff, "Delta mode: off, next, view")

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

func formatFloat(val float64) string {
	s := fmt.Sprintf("%.2f", val)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}
