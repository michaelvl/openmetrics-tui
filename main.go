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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	dto "github.com/prometheus/client_model/go"
)

// Config holds the command line arguments
type Config struct {
	URL          string
	Interval     time.Duration
	History      int
	HideLabels   bool
	FilterMetric string
	FilterLabel  string
	ShowDeltas   bool
}

type model struct {
	cfg               Config
	store             *Store
	fetcher           *Fetcher
	err               error
	width             int
	height            int
	metricNameStyle   lipgloss.Style
	labelStyle        lipgloss.Style
	currentValueStyle lipgloss.Style
	deltaValueStyle   lipgloss.Style
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
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "l":
			m.cfg.HideLabels = !m.cfg.HideLabels
			return m, nil
		case "d":
			m.cfg.ShowDeltas = !m.cfg.ShowDeltas
			return m, nil
		}
	case tickMsg:
		return m, tea.Batch(m.fetchCmd(), m.tickCmd())
	case map[string]*dto.MetricFamily: // Fetch result
		m.store.UpdateFromFamilies(msg)
		return m, nil
	case error:
		m.err = msg
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}

	return m, nil
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\n\nPress q to quit.", m.err)
	}

	// Build the table
	tableStr := m.buildTable()

	// Add a footer with help
	help := "q/ctrl+c: quit | l: toggle labels | d: toggle deltas"
	if m.cfg.ShowDeltas {
		help += " | deltas: on"
	} else {
		help += " | deltas: off"
	}
	if m.cfg.HideLabels {
		help += " | labels: off"
	} else {
		help += " | labels: on"
	}

	return tableStr + "\n" + help + "\n"
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
		// Style metric name and labels
		styledName := m.metricNameStyle.Render(series.Name)
		if !m.cfg.HideLabels && len(series.Labels) > 0 {
			var labelParts []string
			for k, v := range series.Labels {
				labelParts = append(labelParts, fmt.Sprintf("%s=%s", k, v))
			}
			sort.Strings(labelParts)
			styledName = styledName + m.labelStyle.Render(fmt.Sprintf("{%s}", strings.Join(labelParts, ",")))
		}

		row := []string{styledName}

		// Get values - build all possible value columns up to history limit
		vals := series.ValuesWithDeltas(m.cfg.ShowDeltas)
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
					if m.cfg.ShowDeltas && !isCurrentValue {
						// Delta values (not the current value)
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
						// Current value is always shown in magenta
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

	// Pad rows to fill terminal height
	// The table renders with: top border, header, border under header, data rows with borders between them, bottom border
	// Plus help text (2 lines)
	// Be conservative and subtract a bit more to ensure top border shows
	tableOverhead := 5
	dataRows := len(rows)
	totalTableHeight := dataRows + tableOverhead

	if m.height > totalTableHeight {
		emptyRowsNeeded := m.height - totalTableHeight - 1 // Extra safety margin
		if emptyRowsNeeded > 0 {
			emptyRow := make([]string, len(headers))
			for i := range emptyRow {
				emptyRow[i] = ""
			}
			for i := 0; i < emptyRowsNeeded; i++ {
				rows = append(rows, emptyRow)
			}
		}
	}

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
	flag.BoolVar(&cfg.HideLabels, "hide-labels", false, "Hide all labels in the table")
	flag.StringVar(&cfg.FilterMetric, "filter-metric", "", "Regex to filter metrics by name")
	flag.StringVar(&cfg.FilterLabel, "filter-label", "", "Regex to filter metrics by label (e.g. 'env=prod')")
	flag.BoolVar(&cfg.ShowDeltas, "show-deltas", false, "Show deltas instead of absolute values")

	flag.Parse()
	return cfg
}

func formatFloat(val float64) string {
	s := fmt.Sprintf("%.2f", val)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}
