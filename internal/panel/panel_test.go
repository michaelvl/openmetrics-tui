package panel

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

var nan = math.NaN()

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

var testWindows = []dashboard.Window{
	{Label: "now"},
	{Label: "5m", Offset: 5 * time.Minute},
	{Label: "15m", Offset: 15 * time.Minute},
}

// mkSeries builds a series with one point per step ending at t0; vals[i] is
// the sample len(vals)-1-i steps before t0.
func mkSeries(step time.Duration, labels map[string]string, vals ...float64) promapi.Series {
	s := promapi.Series{Labels: labels}
	for i, v := range vals {
		s.Points = append(s.Points, promapi.Sample{T: t0.Add(-time.Duration(len(vals)-1-i) * step), V: v})
	}
	return s
}

func TestPlanRange(t *testing.T) {
	r := PlanRange(testWindows, time.Hour, t0, 120, 15*time.Second)
	if r.Step != 30*time.Second {
		t.Errorf("step %v", r.Step)
	}
	if r.End != t0 || r.End.Sub(r.Start) != time.Hour {
		t.Errorf("range %v..%v", r.Start, r.End)
	}
	for _, w := range testWindows {
		if w.Offset%r.Step != 0 {
			t.Errorf("offset %v not on a %v step", w.Offset, r.Step)
		}
	}

	// Smallest offset 7m: 420s; the divisor nearest the preferred step wins.
	ws := []dashboard.Window{{Label: "now"}, {Label: "7m", Offset: 7 * time.Minute}, {Label: "1h", Offset: time.Hour}}
	r = PlanRange(ws, 0, t0, 100, 15*time.Second)
	if (7*time.Minute)%r.Step != 0 {
		t.Errorf("7m not divisible by %v", r.Step)
	}
	// No nonzero offset: preferred step.
	r = PlanRange([]dashboard.Window{{Label: "now"}}, time.Hour, t0, 120, 15*time.Second)
	if r.Step != 30*time.Second {
		t.Errorf("step %v", r.Step)
	}
	// Span not a multiple of the step: start is moved back to the grid.
	r = PlanRange([]dashboard.Window{{Label: "now"}, {Label: "50s", Offset: 50 * time.Second}}, 70*time.Second, t0, 1, 15*time.Second)
	if r.End.Sub(r.Start)%r.Step != 0 || r.End.Sub(r.Start) < 70*time.Second {
		t.Errorf("range %v..%v step %v", r.Start, r.End, r.Step)
	}
}

func TestExtractColumns(t *testing.T) {
	step := time.Minute
	r := Range{Start: t0.Add(-15 * time.Minute), End: t0, Step: step}
	vals := make([]float64, 16)
	for i := range vals {
		vals[i] = float64(i) // 15 minutes ago is 0, now is 15
	}
	rows := Extract(dashboard.Panel{Label: "{{a}}"}, []promapi.Series{mkSeries(step, map[string]string{"a": "x"}, vals...)}, testWindows, time.Hour, r)
	if len(rows) != 1 || rows[0].Title != "x" {
		t.Fatalf("rows %+v", rows)
	}
	got := []float64{rows[0].Cells[0].V, rows[0].Cells[1].V, rows[0].Cells[2].V}
	if !reflect.DeepEqual(got, []float64{15, 10, 0}) {
		t.Errorf("cells %v", got)
	}
	if !rows[0].Earliest.Equal(t0.Add(-15*time.Minute)) || !rows[0].Latest.Equal(t0) {
		t.Errorf("earliest/latest %v %v", rows[0].Earliest, rows[0].Latest)
	}
	if len(rows[0].Spark) != 61 || !math.IsNaN(rows[0].Spark[0]) || rows[0].Spark[60] != 15 || rows[0].Spark[45] != 0 {
		t.Errorf("spark %v", rows[0].Spark)
	}
}

func TestExtractAgg(t *testing.T) {
	step := time.Minute
	r := Range{Start: t0.Add(-15 * time.Minute), End: t0, Step: step}
	// The 5m window covers the samples from -5m to now: 1..6.
	vals := []float64{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 1, 2, 3, 4, 5, 6}
	for _, agg := range []string{"avg", "max", "min"} {
		rows := Extract(dashboard.Panel{Agg: agg}, []promapi.Series{mkSeries(step, nil, vals...)}, testWindows, 0, r)
		want := map[string]float64{"avg": 3.5, "max": 6, "min": 1}[agg]
		if got := rows[0].Cells[1].V; got != want {
			t.Errorf("%s: got %v want %v", agg, got, want)
		}
		if got := rows[0].Cells[0].V; got != 6 {
			t.Errorf("%s now: %v", agg, got)
		}
		if rows[0].Spark != nil {
			t.Error("spark should be nil")
		}
	}
}

func TestExtractGapsStaleAndBefore(t *testing.T) {
	step := time.Minute
	r := Range{Start: t0.Add(-15 * time.Minute), End: t0, Step: step}

	// Gap at the 5m column and NaN samples: column is NaN, not Before.
	s := mkSeries(step, nil, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, nan, 1, 1, 1, 1, 7)
	rows := Extract(dashboard.Panel{}, []promapi.Series{s}, testWindows, 0, r)
	if !math.IsNaN(rows[0].Cells[1].V) || rows[0].Cells[1].Before || rows[0].Cells[2].V != 1 {
		t.Errorf("gap: %+v", rows[0].Cells)
	}
	if rows[0].Cells[0].V != 7 {
		t.Errorf("now %v", rows[0].Cells[0].V)
	}

	// Trailing NaN stale marker: now is the last non-NaN point if recent.
	s = mkSeries(step, nil, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 4, 5, nan)
	rows = Extract(dashboard.Panel{}, []promapi.Series{s}, testWindows, 0, r)
	if rows[0].Cells[0].V != 5 {
		t.Errorf("stale NaN: now %v", rows[0].Cells[0].V)
	}

	// Series that disappeared 10 minutes ago: no now value, still has a 15m value.
	s = promapi.Series{Points: mkSeries(step, nil, 1, 1, 1, 1, 1, 2, 2, 2, 2, 2).Points}
	for i := range s.Points {
		s.Points[i].T = s.Points[i].T.Add(-10 * time.Minute)
	}
	rows = Extract(dashboard.Panel{}, []promapi.Series{s}, testWindows, 0, r)
	if !math.IsNaN(rows[0].Cells[0].V) {
		t.Errorf("gone series has now %v", rows[0].Cells[0].V)
	}
	if !rows[0].Latest.Equal(t0.Add(-10 * time.Minute)) {
		t.Errorf("latest %v", rows[0].Latest)
	}

	// Series that appeared 8 minutes ago: 15m column is Before, 5m has a value.
	s = mkSeries(step, nil, 1, 1, 1, 1, 1, 1, 1, 1, 3)
	rows = Extract(dashboard.Panel{}, []promapi.Series{s}, testWindows, 0, r)
	if !rows[0].Cells[2].Before || !math.IsNaN(rows[0].Cells[2].V) || rows[0].Cells[1].Before || rows[0].Cells[1].V != 1 {
		t.Errorf("appeared: %+v", rows[0].Cells)
	}
	// Same with agg.
	rows = Extract(dashboard.Panel{Agg: "max"}, []promapi.Series{s}, testWindows, 0, r)
	if !rows[0].Cells[2].Before {
		t.Errorf("agg before: %+v", rows[0].Cells)
	}

	// Only NaN, and no points at all.
	rows = Extract(dashboard.Panel{}, []promapi.Series{mkSeries(step, nil, nan, nan), {}}, testWindows, time.Hour, r)
	for _, row := range rows {
		if !row.Earliest.IsZero() || !math.IsNaN(row.Cells[0].V) || row.Cells[1].Before {
			t.Errorf("empty row %+v", row)
		}
	}
	if !Earliest(rows).IsZero() {
		t.Error("Earliest of empty rows")
	}
}

func TestEarliest(t *testing.T) {
	rows := []Row{{Earliest: t0}, {}, {Earliest: t0.Add(-time.Minute)}}
	if !Earliest(rows).Equal(t0.Add(-time.Minute)) {
		t.Error(Earliest(rows))
	}
}

func TestColor(t *testing.T) {
	ts := []dashboard.Threshold{
		{Op: "gt", Value: 5, Color: "red"},
		{Op: "gte", Value: 1, Color: "yellow"},
		{Color: "green"},
	}
	for v, want := range map[float64]string{9: "red", 5: "yellow", 1: "yellow", 0.5: "green", -3: "green", nan: ""} {
		if got := Color(ts, v); got != want {
			t.Errorf("%v: got %q want %q", v, got, want)
		}
	}
	lt := []dashboard.Threshold{{Op: "lt", Value: 1, Color: "red"}, {Op: "lte", Value: 2, Color: "yellow"}}
	for v, want := range map[float64]string{0.9: "red", 1: "yellow", 2: "yellow", 2.1: ""} {
		if got := Color(lt, v); got != want {
			t.Errorf("%v: got %q want %q", v, got, want)
		}
	}
	if Color(nil, 1) != "" {
		t.Error("no thresholds")
	}
}

func TestLabel(t *testing.T) {
	l := map[string]string{"controller": "gadget", "namespace": "prod"}
	for tmpl, want := range map[string]string{
		"{{controller}}":                 "gadget",
		"{{ controller }}/{{namespace}}": "gadget/prod",
		"ctl: {{controller}}!":           "ctl: gadget!",
		"plain":                          "plain",
		"":                               "{controller=gadget,namespace=prod}",
		"{{missing}}":                    "{controller=gadget,namespace=prod}",
		"{{open":                         "{controller=gadget,namespace=prod}",
	} {
		if got := Label(tmpl, l); got != want {
			t.Errorf("%q: got %q want %q", tmpl, got, want)
		}
	}
	if got := Label("", nil); got != "{}" {
		t.Errorf("no labels: %q", got)
	}
}

func TestFormatValue(t *testing.T) {
	for _, c := range []struct {
		unit string
		v    float64
		want string
	}{
		{"percent", 0.014, "1.4%"}, {"percent", 0, "0%"}, {"percent", 1, "100%"},
		{"s", 0.21, "0.21"}, {"s", 0.0034, "0.0034"}, {"/min", 3.2, "3.2"},
		{"", 21203, "21203"}, {"", 143, "143"}, {"", 12.345, "12.3"}, {"", nan, "-"},
		{"percent", nan, "-"}, {"", math.Inf(1), "+Inf"},
	} {
		if got := FormatValue(c.unit, c.v); got != c.want {
			t.Errorf("%q %v: got %q want %q", c.unit, c.v, got, c.want)
		}
	}
}

func rowsNow(titles []string, vals ...float64) []Row {
	rows := make([]Row, len(vals))
	for i, v := range vals {
		rows[i] = Row{Title: titles[i], Cells: []Cell{{V: v}}}
	}
	return rows
}

func titlesOf(rows []Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

func TestArrange(t *testing.T) {
	ws := testWindows[:1]
	rows := rowsNow([]string{"b", "c", "a", "d"}, 2, nan, 3, 1)

	check := func(p dashboard.Panel, mode SortMode, want ...string) {
		t.Helper()
		if got := titlesOf(Arrange(rows, p, ws, mode)); !reflect.DeepEqual(got, want) {
			t.Errorf("%+v mode %d: got %v want %v", p, mode, got, want)
		}
	}
	check(dashboard.Panel{}, SortYAML, "b", "c", "a", "d")
	check(dashboard.Panel{Sort: "desc"}, SortYAML, "a", "b", "d", "c")
	check(dashboard.Panel{Sort: "asc"}, SortYAML, "d", "b", "a", "c")
	check(dashboard.Panel{Sort: "desc", Limit: 2}, SortYAML, "a", "b")
	check(dashboard.Panel{Sort: "asc"}, SortNowDesc, "a", "b", "d", "c")
	check(dashboard.Panel{Sort: "desc"}, SortNowAsc, "d", "b", "a", "c")
	check(dashboard.Panel{Limit: 3}, SortLabel, "a", "b", "c")
	if got := titlesOf(rows); !reflect.DeepEqual(got, []string{"b", "c", "a", "d"}) {
		t.Errorf("input mutated: %v", got)
	}
	if SortLabel.Next() != SortYAML || SortYAML.Next() != SortNowDesc {
		t.Error("cycle")
	}
}

var rgy = []dashboard.Threshold{
	{Op: "gt", Value: 5, Color: "red"},
	{Op: "gt", Value: 1, Color: "yellow"},
	{Color: "green"},
}

func TestSummarize(t *testing.T) {
	ws := testWindows[:1]
	names := []string{"a", "b", "c", "d", "e"}
	p := dashboard.Panel{Thresholds: rgy}

	s := Summarize(p, ws, rowsNow(names, 0.1, 3, 7.9, 9.1, 2))
	wantCounts := []ColorCount{{"red", 2}, {"yellow", 2}, {"green", 1}}
	if !reflect.DeepEqual(s.Counts, wantCounts) || s.Worst != 3 || s.WorstColor != "red" || s.WorstValue != 9.1 || s.Rows != 5 {
		t.Errorf("mixed: %+v", s)
	}

	// Tie on value: the earlier row wins.
	s = Summarize(p, ws, rowsNow(names, 7, 7, 1))
	if s.Worst != 0 {
		t.Errorf("tie: %+v", s)
	}

	// No red: worst is the highest yellow.
	s = Summarize(p, ws, rowsNow(names, 0.1, 2, 4))
	if s.WorstColor != "yellow" || s.Worst != 2 {
		t.Errorf("yellow: %+v", s)
	}

	// Unknown colours rank below green; with only unknown, highest value wins.
	custom := dashboard.Panel{Thresholds: []dashboard.Threshold{{Op: "gt", Value: 5, Color: "purple"}, {Color: "green"}}}
	s = Summarize(custom, ws, rowsNow(names, 9, 1))
	if s.WorstColor != "green" || s.Worst != 1 || s.Counts[0].Color != "green" || s.Counts[1].Color != "purple" {
		t.Errorf("custom: %+v", s)
	}
	only := dashboard.Panel{Thresholds: []dashboard.Threshold{{Color: "teal"}}}
	s = Summarize(only, ws, rowsNow(names, 1, 4, 2))
	if s.Worst != 1 || s.WorstColor != "teal" {
		t.Errorf("only custom: %+v", s)
	}

	// No thresholds: row count only.
	s = Summarize(dashboard.Panel{}, ws, rowsNow(names, 1, 2, 3))
	if s.Rows != 3 || s.Single || len(s.Counts) != 0 || s.Worst != -1 {
		t.Errorf("no thresholds: %+v", s)
	}

	// Single series: just its value, even with thresholds.
	s = Summarize(p, ws, rowsNow(names, 0.21))
	if !s.Single || s.Value != 0.21 {
		t.Errorf("single: %+v", s)
	}

	// NaN-only rows are not counted and give no worst.
	s = Summarize(p, ws, rowsNow(names, nan, nan, nan))
	if s.Rows != 3 || len(s.Counts) != 0 || s.Worst != -1 {
		t.Errorf("nan only: %+v", s)
	}
	s = Summarize(p, ws, rowsNow(names, nan))
	if !s.Single || !math.IsNaN(s.Value) {
		t.Errorf("single nan: %+v", s)
	}

	// Empty results.
	s = Summarize(p, ws, nil)
	if s.Rows != 0 || s.Single || len(s.Counts) != 0 || s.Worst != -1 {
		t.Errorf("empty: %+v", s)
	}
}

func TestSparkline(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []float64
		n    int
		want string
	}{
		{"ramp", []float64{0, 1, 2, 3, 4, 5, 6, 7}, 8, "▁▂▃▄▅▆▇█"},
		{"downsample averages", []float64{0, 0, 7, 7}, 2, "▁█"},
		{"upsample stretches", []float64{0, 7}, 4, "▁▁██"},
		{"flat", []float64{3, 3, 3, 3}, 4, "▁▁▁▁"},
		{"flat zero", []float64{0, 0}, 3, "▁▁▁"},
		{"single value", []float64{5}, 3, "▁▁▁"},
		{"empty", nil, 4, "...."},
		{"all nan", []float64{nan, nan}, 3, "..."},
		{"gap", []float64{0, nan, 7}, 3, "▁.█"},
		{"inf missing", []float64{0, math.Inf(1), 7}, 3, "▁.█"},
		{"zero width", []float64{1, 2}, 0, ""},
	} {
		if got := Sparkline(c.in, c.n); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
