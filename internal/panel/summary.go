package panel

import (
	"math"
	"sort"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
)

// ColorCount is the number of rows in one threshold colour.
type ColorCount struct {
	Color string
	N     int
}

// Summary is what a folded panel shows. Exactly one of three shapes applies:
// Single (one series: just its value), Counts non-empty (thresholds: rows per
// colour and the worst row), or only Rows (no thresholds: a row count).
type Summary struct {
	Rows int
	// Single is set for a one-series panel; Value is its now value (maybe NaN).
	Single bool
	Value  float64
	// Counts lists colours by severity, then other colours alphabetically.
	// Rows with no now value or no matching threshold are not counted.
	Counts []ColorCount
	// Worst indexes the worst row in the rows passed to Summarize, or -1.
	Worst      int
	WorstColor string
	WorstValue float64
}

// severity ranks threshold colours; unknown and custom colours rank lowest.
func severity(color string) int {
	switch color {
	case "red":
		return 3
	case "yellow":
		return 2
	case "green":
		return 1
	}
	return 0
}

// Summarize computes the folded summary of rows from the now column. The
// worst row has the highest severity (red, yellow, green, then anything
// else); ties go to the highest value, then to the earlier row.
func Summarize(p dashboard.Panel, windows []dashboard.Window, rows []Row) Summary {
	s := Summary{Rows: len(rows), Worst: -1, Value: math.NaN()}
	if len(rows) == 1 {
		s.Single = true
		s.Value = rows[0].Now(windows)
		return s
	}
	if len(p.Thresholds) == 0 {
		return s
	}
	counts := map[string]int{}
	for i, r := range rows {
		v := r.Now(windows)
		c := Color(p.Thresholds, v)
		if c == "" {
			continue
		}
		counts[c]++
		if s.Worst < 0 || severity(c) > severity(s.WorstColor) ||
			(severity(c) == severity(s.WorstColor) && v > s.WorstValue) {
			s.Worst, s.WorstColor, s.WorstValue = i, c, v
		}
	}
	for c, n := range counts {
		s.Counts = append(s.Counts, ColorCount{Color: c, N: n})
	}
	sort.Slice(s.Counts, func(a, b int) bool {
		ca, cb := s.Counts[a], s.Counts[b]
		if sa, sb := severity(ca.Color), severity(cb.Color); sa != sb {
			return sa > sb
		}
		return ca.Color < cb.Color
	})
	return s
}
