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

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	dto "github.com/prometheus/client_model/go"
)

// Config holds the command line arguments
type Config struct {
	URL          string
	Interval     time.Duration
	History      int
	ShowLabels   bool
	FilterMetric string
	FilterLabel  string
	ShowDeltas   bool
}

type model struct {
	cfg     Config
	store   *Store
	fetcher *Fetcher
	table   table.Model
	err     error
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

	// Initialize table
	metricWidth := 30
	if cfg.ShowLabels {
		metricWidth = 60
	}
	columns := []table.Column{
		{Title: "Metric", Width: metricWidth},
		{Title: "Value", Width: 15},
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(10),
	)

	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Bold(false)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).
		Bold(false)
	t.SetStyles(s)

	m := model{
		cfg:     cfg,
		store:   store,
		fetcher: fetcher,
		table:   t,
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
		}
	case tickMsg:
		return m, tea.Batch(m.fetchCmd(), m.tickCmd())
	case map[string]*dto.MetricFamily: // Fetch result
		m.store.UpdateFromFamilies(msg)
		m.updateTable()
		return m, nil
	case error:
		m.err = msg
		return m, nil
	case tea.WindowSizeMsg:
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(msg.Height - 5) // Reserve space for header/footer
		m.updateTable()
	}

	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\n\nPress q to quit.", m.err)
	}
	
	// Add a footer with help
	help := "q/ctrl+c: quit | arrows: navigate"
	if m.cfg.ShowDeltas {
		help += " | deltas: on"
	} else {
		help += " | deltas: off"
	}
	
	return baseStyle.Render(m.table.View()) + "\n" + help + "\n"
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

func (m *model) updateTable() {
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

	// Calculate max widths based on filtered data
	maxValueWidth := 5
	metricColWidth := 30

	for _, series := range filteredSeries {
		// Calculate metric name width
		name := series.Name
		if m.cfg.ShowLabels && len(series.Labels) > 0 {
			var labelParts []string
			for k, v := range series.Labels {
				labelParts = append(labelParts, fmt.Sprintf("%s=%s", k, v))
			}
			sort.Strings(labelParts)
			name = fmt.Sprintf("%s{%s}", series.Name, strings.Join(labelParts, ","))
		}
		if len(name) > metricColWidth {
			metricColWidth = len(name)
		}

		vals := series.ValuesWithDeltas(m.cfg.ShowDeltas)
		for i, val := range vals {
			if math.IsNaN(val) {
				continue
			}
			formatted := formatFloat(val)
			if m.cfg.ShowDeltas && i < len(vals)-1 {
				if formatted == "0" || formatted == "-0" {
					formatted = "."
				} else {
					formatted = "Δ" + formatted
				}
			}
			if len(formatted) > maxValueWidth {
				maxValueWidth = len(formatted)
			}
		}
	}

	// Calculate columns
	width := m.table.Width()
	
	cols := []table.Column{
		{Title: "Metric", Width: metricColWidth},
	}
	
	usedWidth := metricColWidth + 4
	availableForValues := width - usedWidth
	// Handle edge case where width is not yet set
	if width == 0 {
		availableForValues = 100 // Default
	}
	
	numValueCols := availableForValues / (maxValueWidth + 2)
	if numValueCols > m.cfg.History {
		numValueCols = m.cfg.History
	}
	if numValueCols < 1 {
		numValueCols = 1
	}
	
	for i := 0; i < numValueCols; i++ {
		title := fmt.Sprintf("-%ds", (numValueCols-1-i)*int(m.cfg.Interval.Seconds()))
		if i == numValueCols-1 {
			title = "Curr"
		}
		cols = append(cols, table.Column{Title: title, Width: maxValueWidth})
	}
	// Clear rows to avoid panic if new columns > old rows
	m.table.SetRows([]table.Row{})
	m.table.SetColumns(cols)

	rows := []table.Row{}
	
	for _, series := range filteredSeries {
		name := series.Name
		if m.cfg.ShowLabels && len(series.Labels) > 0 {
			// Format labels nicely
			var labelParts []string
			for k, v := range series.Labels {
				labelParts = append(labelParts, fmt.Sprintf("%s=%s", k, v))
			}
			sort.Strings(labelParts)
			name = fmt.Sprintf("%s{%s}", series.Name, strings.Join(labelParts, ","))
		}
		
		row := []string{name}

		// Get values
		vals := series.ValuesWithDeltas(m.cfg.ShowDeltas)
		
		// Create a slice of strings for the value columns
		valStrs := make([]string, numValueCols)
		for i := 0; i < numValueCols; i++ {
			// Map column index to value index
			// Column 0 is oldest displayed. Column numValueCols-1 is newest.
			// We want to display the last numValueCols values.
			// The value at index `len(vals) - 1` should go to column `numValueCols - 1`.
			// The value at index `len(vals) - 1 - offset` should go to column `numValueCols - 1 - offset`.
			
			offset := numValueCols - 1 - i
			valIdx := len(vals) - 1 - offset
			
			if valIdx >= 0 && valIdx < len(vals) {
				val := vals[valIdx]
				if math.IsNaN(val) {
					valStrs[i] = "."
				} else {
					formatted := formatFloat(val)
					if m.cfg.ShowDeltas && valIdx < len(vals)-1 {
						if formatted == "0" || formatted == "-0" {
							formatted = "."
						} else {
							formatted = "Δ" + formatted
						}
					}
					valStrs[i] = formatted
				}
			} else {
				valStrs[i] = ""
			}
		}
		
		row = append(row, valStrs...)
		rows = append(rows, table.Row(row))
	}
	
	m.table.SetRows(rows)
}

func parseFlags() Config {
	var cfg Config
	flag.StringVar(&cfg.URL, "url", "", "URL to poll metrics from (required)")
	flag.DurationVar(&cfg.Interval, "interval", 5*time.Second, "Polling interval")
	flag.IntVar(&cfg.History, "history", 10, "Number of historical samples to keep")
	flag.BoolVar(&cfg.ShowLabels, "show-labels", false, "Show all labels in the table")
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

