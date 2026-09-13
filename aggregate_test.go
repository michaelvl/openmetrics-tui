package main

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestParseAggregationDefaultsToSum(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		active bool
		op     aggOp
		by     []string
	}{
		{"empty is off", "", false, aggSum, nil},
		{"blank is off", "   ", false, aggSum, nil},
		{"bare list sums", "pod", true, aggSum, []string{"pod"}},
		{"several labels", "pod,instance", true, aggSum, []string{"pod", "instance"}},
		{"spaces around labels", " pod , instance ", true, aggSum, []string{"pod", "instance"}},
		{"operator prefix", "avg pod", true, aggAvg, []string{"pod"}},
		{"min prefix", "min pod,instance", true, aggMin, []string{"pod", "instance"}},
		{"max prefix", "max pod", true, aggMax, []string{"pod"}},
		{"count prefix", "count pod", true, aggCount, []string{"pod"}},
		// A lone operator groups by nothing at all, which totals every series of
		// a metric into one row.
		{"operator alone", "count", true, aggCount, nil},
		{"sum alone", "sum", true, aggSum, nil},
		// The escape hatch for a label whose name collides with an operator.
		{"label named sum", "sum sum", true, aggSum, []string{"sum"}},

		// The PromQL spelling, which is what anyone who knows PromQL types first.
		// The "by" and the parentheses are both optional decoration.
		{"promql", "sum by (pod)", true, aggSum, []string{"pod"}},
		{"promql without a space", "sum by(pod)", true, aggSum, []string{"pod"}},
		{"promql without parens", "avg by pod", true, aggAvg, []string{"pod"}},
		{"promql without an operator", "by (pod)", true, aggSum, []string{"pod"}},
		{"promql list", "sum by (pod, instance)", true, aggSum, []string{"pod", "instance"}},
		{"parens alone", "(pod)", true, aggSum, []string{"pod"}},
		// sum by () is PromQL for grouping by nothing, which is the total.
		{"empty parens", "sum by ()", true, aggSum, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAggregation(tt.value)
			if got.active != tt.active || got.op != tt.op || !reflect.DeepEqual(got.by, tt.by) {
				t.Errorf("parseAggregation(%q) = {%v %v %v}, want {%v %v %v}",
					tt.value, got.active, got.op, got.by, tt.active, tt.op, tt.by)
			}
		})
	}
}

func TestValidateAggregation(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty", "", false},
		{"bare label", "pod", false},
		{"label list", "pod,instance", false},
		{"operator and label", "avg pod", false},
		{"operator alone", "count", false},
		{"label named after an operator", "sum sum", false},
		{"underscored label", "_pod_2", false},

		// A space after the separator is ordinary typing, not an operator.
		{"spaced list", "pod, instance", false},
		{"space before the separator", "pod ,instance", false},

		{"promql by", "sum by (env)", false},
		{"promql by without a space", "sum by(env)", false},
		{"promql by without parens", "sum by env", false},
		{"bare promql by", "by (env)", false},
		{"promql empty by", "sum by ()", false},

		{"misspelled operator", "avgg pod", true},
		// "without" keeps the labels it is not given, which is the opposite
		// selection and is not implemented.
		{"promql without", "sum without (env)", true},
		{"bare promql without", "without (env)", true},
		{"trailing comma", "pod,", true},
		{"doubled comma", "pod,,instance", true},
		{"leading comma", ",pod", true},
		{"label starting with a digit", "2pod", true},
		{"label with a dash", "pod-name", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAggregation(tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateAggregation(%q) = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

// series is a terse constructor for the aggregation tests.
func series(name string, labels map[string]string, values ...float64) *MetricSeries {
	return &MetricSeries{Name: name, Kind: KindGauge, Labels: labels, Values: values}
}

func TestAggregateGroupsByTheNamedLabels(t *testing.T) {
	in := []*MetricSeries{
		series("queue_depth", map[string]string{"env": "prod", "pod": "a"}, 1),
		series("queue_depth", map[string]string{"env": "prod", "pod": "b"}, 2),
		series("queue_depth", map[string]string{"env": "dev", "pod": "a"}, 4),
	}

	got := parseAggregation("env").aggregate(in)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want one per env", len(got))
	}
	// Sorted by signature, so dev precedes prod.
	if got[0].Labels["env"] != "dev" || got[0].Values[0] != 4 {
		t.Errorf("dev row = %v %v, want env=dev and 4", got[0].Labels, got[0].Values)
	}
	if got[1].Labels["env"] != "prod" || got[1].Values[0] != 3 {
		t.Errorf("prod row = %v %v, want env=prod and 3", got[1].Labels, got[1].Values)
	}
	if _, ok := got[1].Labels["pod"]; ok {
		t.Errorf("aggregated-away label survived in %v", got[1].Labels)
	}
	if got[1].Name != "sum(queue_depth)" {
		t.Errorf("Name = %q, want the operator to name the row", got[1].Name)
	}
}

func TestAggregateWithNoLabelsTotalsTheMetric(t *testing.T) {
	in := []*MetricSeries{
		series("queue_depth", map[string]string{"pod": "a"}, 1),
		series("queue_depth", map[string]string{"pod": "b"}, 2),
	}

	got := parseAggregation("sum").aggregate(in)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want one", len(got))
	}
	if got[0].Values[0] != 3 {
		t.Errorf("Values = %v, want 3", got[0].Values)
	}
	if len(got[0].Labels) != 0 {
		t.Errorf("Labels = %v, want none", got[0].Labels)
	}
}

// PromQL's sum by (pod) drops the metric name and would add these two together.
// Inside one exporter that is never what was meant.
func TestAggregateNeverMergesMetricNames(t *testing.T) {
	in := []*MetricSeries{
		series("queue_depth", map[string]string{"pod": "a"}, 1),
		series("worker_count", map[string]string{"pod": "a"}, 2),
	}

	got := parseAggregation("sum").aggregate(in)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want one per metric name:\n%v", len(got), got)
	}
}

// A series first seen mid-run has a shorter Values slice whose last element is
// still this scrape, so the fold has to line the slices up at their newest end.
// Aligning from the oldest would report b's 30 as if it were three scrapes ago.
func TestAggregateAlignsHistoryFromTheNewestEnd(t *testing.T) {
	in := []*MetricSeries{
		series("queue_depth", map[string]string{"pod": "a"}, 1, 2, 3, 4),
		series("queue_depth", map[string]string{"pod": "b"}, 30, 40),
	}

	got := parseAggregation("sum").aggregate(in)
	want := []float64{1, 2, 33, 44}
	if !reflect.DeepEqual(got[0].Values, want) {
		t.Errorf("Values = %v, want %v", got[0].Values, want)
	}
}

func TestAggregateSkipsMembersThatDidNotReport(t *testing.T) {
	nan := math.NaN()
	in := []*MetricSeries{
		series("queue_depth", map[string]string{"pod": "a"}, 1, nan, nan),
		series("queue_depth", map[string]string{"pod": "b"}, 10, 20, nan),
	}

	got := parseAggregation("sum").aggregate(in)[0].Values
	if got[0] != 11 {
		t.Errorf("Values[0] = %v, want 11", got[0])
	}
	if got[1] != 20 {
		t.Errorf("Values[1] = %v, want the one member that reported", got[1])
	}
	// Nobody reported, so there is nothing to total - not a total of zero.
	if !math.IsNaN(got[2]) {
		t.Errorf("Values[2] = %v, want NaN", got[2])
	}
}

func TestAggregateOperators(t *testing.T) {
	nan := math.NaN()
	group := func() []*MetricSeries {
		return []*MetricSeries{
			series("queue_depth", map[string]string{"pod": "a"}, 2, nan),
			series("queue_depth", map[string]string{"pod": "b"}, 4, nan),
			series("queue_depth", map[string]string{"pod": "c"}, 12, 5),
		}
	}

	tests := []struct {
		value string
		want  float64
		name  string
	}{
		{"sum", 18, "sum(queue_depth)"},
		{"avg", 6, "avg(queue_depth)"},
		{"min", 2, "min(queue_depth)"},
		{"max", 12, "max(queue_depth)"},
		{"count", 3, "count(queue_depth)"},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got := parseAggregation(tt.value).aggregate(group())[0]
			if got.Values[0] != tt.want {
				t.Errorf("%s of 2,4,12 = %v, want %v", tt.value, got.Values[0], tt.want)
			}
			if got.Name != tt.name {
				t.Errorf("Name = %q, want %q", got.Name, tt.name)
			}
		})
	}

	// Only c reported at the second scrape, so every operator answers about c
	// alone - including count, which is 1 and not 3.
	for _, tt := range []struct {
		value string
		want  float64
	}{{"sum", 5}, {"avg", 5}, {"min", 5}, {"max", 5}, {"count", 1}} {
		got := parseAggregation(tt.value).aggregate(group())[0].Values[1]
		if got != tt.want {
			t.Errorf("%s with one reporting member = %v, want %v", tt.value, got, tt.want)
		}
	}
}

// Aggregation is a no-op when the field is empty, and must hand back the very
// series it was given rather than copies - the store owns them.
func TestAggregateIsANoOpWhenOff(t *testing.T) {
	in := []*MetricSeries{series("queue_depth", map[string]string{"pod": "a"}, 1)}
	got := parseAggregation("").aggregate(in)
	if len(got) != 1 || got[0] != in[0] {
		t.Errorf("aggregate with no spec did not pass the input through")
	}
}

// hide-static has to judge the aggregated row, not its members: the sum over a
// fixed series and a moving one moves, and dropping the fixed member first would
// both hide that row and understate the total of the rows that survived.
func TestHideStaticJudgesTheAggregatedRow(t *testing.T) {
	first := `# TYPE queue_depth gauge
queue_depth{pod="a"} 5
queue_depth{pod="b"} 1
`
	second := `# TYPE queue_depth gauge
queue_depth{pod="a"} 5
queue_depth{pod="b"} 2
`
	m := headerModel(t, 80, first, second)
	m.cfg.HideStatic = true

	// Without aggregation the fixed series is hidden and the moving one is not.
	table := plain(m.buildTable())
	if strings.Contains(table, "pod=a") || !strings.Contains(table, "pod=b") {
		t.Errorf("hide-static did not hide the fixed series alone:\n%s", table)
	}

	m.cfg.Aggregation = "sum"
	table = plain(m.buildTable())
	if !strings.Contains(table, "sum(queue_depth)") {
		t.Errorf("the aggregated row moves 6 -> 7 and should have survived hide-static:\n%s", table)
	}
	if !strings.Contains(table, "7") {
		t.Errorf("total does not include the fixed member:\n%s", table)
	}
}

// A series lacking a grouping label must not be folded in with the ones that
// have it, or "sum by pod" would quietly add an unlabelled total to a per-pod one.
func TestAggregateSeparatesSeriesLackingTheLabel(t *testing.T) {
	in := []*MetricSeries{
		series("queue_depth", map[string]string{"pod": "a"}, 1),
		series("queue_depth", map[string]string{}, 100),
	}

	got := parseAggregation("pod").aggregate(in)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the unlabelled series kept apart", len(got))
	}
	for _, row := range got {
		if len(row.Labels) == 0 && row.Values[0] != 100 {
			t.Errorf("unlabelled row = %v, want 100", row.Values)
		}
	}
}
