package main

import (
	"math"
	"reflect"
	"sort"
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

// perPodLatency is two targets reporting the same histogram, with a bucket layout
// they share and counts that differ.
const perPodLatency = `# TYPE lat_seconds histogram
lat_seconds_bucket{pod="a",le="1"} 2
lat_seconds_bucket{pod="a",le="+Inf"} 4
lat_seconds_sum{pod="a"} 3
lat_seconds_count{pod="a"} 4
lat_seconds_bucket{pod="b",le="1"} 6
lat_seconds_bucket{pod="b",le="+Inf"} 10
lat_seconds_sum{pod="b"} 7
lat_seconds_count{pod="b"} 10
`

// aggregatedDists is the visible list under an aggregation field, which is where
// the distribution view applies it.
func aggregatedDists(t *testing.T, field string, texts ...string) []distEntry {
	t.Helper()
	m := distModel(t, 120, texts...)
	m.cfg.Aggregation = field
	return m.visibleDistributions()
}

// bucketValues reads a family's newest scrape, bound by bound, for comparison
// against what the members reported.
func bucketValues(dist *DistributionSeries) map[string]float64 {
	total := distScrapeCount(dist)
	got := make(map[string]float64, len(dist.Points))
	for _, point := range dist.Points {
		got[formatBound(point.Bound)] = valueAt(point.Series, total-1, total)
	}
	return got
}

// Adding two histograms bucket by bucket is the one fold that yields a histogram:
// the counts, the sum and the buckets are all additive.
func TestDistributionsSumBucketByBucket(t *testing.T) {
	got := aggregatedDists(t, "sum", perPodLatency)
	if len(got) != 1 {
		t.Fatalf("got %d families, want the two pods folded into one: %v", len(got), got)
	}
	dist := got[0].dist
	if dist.Name != "sum(lat_seconds)" {
		t.Errorf("Name = %q, want the operator to name the family", dist.Name)
	}
	if _, ok := dist.Labels["pod"]; ok {
		t.Errorf("aggregated-away label survived in %v", dist.Labels)
	}

	want := map[string]float64{"1": 8, "+Inf": 14}
	if buckets := bucketValues(dist); !reflect.DeepEqual(buckets, want) {
		t.Errorf("buckets = %v, want %v", buckets, want)
	}

	total := distScrapeCount(dist)
	if count := valueAt(dist.Count, total-1, total); count != 14 {
		t.Errorf("count = %v, want 14", count)
	}
	if sum := valueAt(dist.Sum, total-1, total); sum != 10 {
		t.Errorf("sum = %v, want 10", sum)
	}
}

func TestDistributionSumGroupsByTheNamedLabels(t *testing.T) {
	const twoEnvs = `# TYPE lat_seconds histogram
lat_seconds_bucket{env="prod",pod="a",le="+Inf"} 4
lat_seconds_count{env="prod",pod="a"} 4
lat_seconds_bucket{env="prod",pod="b",le="+Inf"} 10
lat_seconds_count{env="prod",pod="b"} 10
lat_seconds_bucket{env="dev",pod="c",le="+Inf"} 1
lat_seconds_count{env="dev",pod="c"} 1
`
	got := aggregatedDists(t, "env", twoEnvs)
	if len(got) != 2 {
		t.Fatalf("got %d families, want one per env: %v", len(got), got)
	}
	// Sorted by signature, so dev precedes prod.
	if got[0].dist.Labels["env"] != "dev" || bucketValues(got[0].dist)["+Inf"] != 1 {
		t.Errorf("dev family = %v %v, want env=dev and 1", got[0].dist.Labels, bucketValues(got[0].dist))
	}
	if got[1].dist.Labels["env"] != "prod" || bucketValues(got[1].dist)["+Inf"] != 14 {
		t.Errorf("prod family = %v %v, want env=prod and 14", got[1].dist.Labels, bucketValues(got[1].dist))
	}
}

// Only sum yields a histogram. An avg or a max of two bucket counts is the
// histogram of nothing, and a p99 read off it would describe a distribution that
// never existed - so the list is left alone and the header says why.
func TestOnlySumFoldsDistributions(t *testing.T) {
	for _, field := range []string{"avg pod", "min pod", "max pod", "count pod"} {
		t.Run(field, func(t *testing.T) {
			got := aggregatedDists(t, field, perPodLatency)
			if len(got) != 2 {
				t.Fatalf("got %d families, want both pods left alone: %v", len(got), got)
			}
			for _, entry := range got {
				if strings.Contains(entry.dist.Name, "(") {
					t.Errorf("Name = %q, want an untouched family", entry.dist.Name)
				}
			}

			m := distModel(t, 120, perPodLatency)
			m.cfg.Aggregation = field
			header := plain(strings.SplitN(mustRender(t, m), "\n", 2)[0])
			if !strings.Contains(header, "ignored: sum only") {
				t.Errorf("header does not say the operator was ignored:\n%s", header)
			}
		})
	}
}

// A summary's points are quantiles the exporter already reduced, and no
// arithmetic on two p99s gives the p99 of the combined stream. The histograms
// around it still fold.
func TestSummariesAreNotSummed(t *testing.T) {
	m := distModel(t, 120, perPodLatency+summaryFixture)
	m.cfg.Aggregation = "sum"
	got := m.visibleDistributions()
	if len(got) != 2 {
		t.Fatalf("got %d families, want the folded histogram and the summary: %v", len(got), got)
	}

	var names []string
	for _, entry := range got {
		names = append(names, entry.dist.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"rpc_seconds", "sum(lat_seconds)"}) {
		t.Errorf("names = %v, want the summary untouched beside the folded histogram", names)
	}

	header := plain(strings.SplitN(mustRender(t, m), "\n", 2)[0])
	if !strings.Contains(header, "summaries not summed") {
		t.Errorf("header does not say the summary was left alone:\n%s", header)
	}
}

// Bucket layouts that differ are unioned, so a bound only one member reports
// still gets a row rather than taking the whole family down with it.
func TestFoldedBucketLayoutsAreUnioned(t *testing.T) {
	const disjoint = `# TYPE lat_seconds histogram
lat_seconds_bucket{pod="a",le="1"} 2
lat_seconds_bucket{pod="a",le="+Inf"} 4
lat_seconds_count{pod="a"} 4
lat_seconds_bucket{pod="b",le="2"} 6
lat_seconds_bucket{pod="b",le="+Inf"} 10
lat_seconds_count{pod="b"} 10
`
	got := aggregatedDists(t, "sum", disjoint)
	if len(got) != 1 {
		t.Fatalf("got %d families, want one: %v", len(got), got)
	}

	// Each of the two odd bounds is summed over the one member that carries it,
	// and +Inf over both. The order is ascending with +Inf last.
	var bounds []string
	for _, point := range got[0].dist.Points {
		bounds = append(bounds, formatBound(point.Bound))
	}
	if !reflect.DeepEqual(bounds, []string{"1", "2", "+Inf"}) {
		t.Errorf("bounds = %v, want 1, 2, +Inf", bounds)
	}
	want := map[string]float64{"1": 2, "2": 6, "+Inf": 14}
	if buckets := bucketValues(got[0].dist); !reflect.DeepEqual(buckets, want) {
		t.Errorf("buckets = %v, want %v", buckets, want)
	}
}

// A target first scraped mid-run has a shorter history, and its newest value is
// still this scrape. Folding has to line the members up at that end, or the new
// pod's counts would be added to the wrong columns.
func TestFoldedDistributionAlignsHistoryFromTheNewestEnd(t *testing.T) {
	const first = `# TYPE lat_seconds histogram
lat_seconds_bucket{pod="a",le="+Inf"} 4
lat_seconds_count{pod="a"} 4
`
	got := aggregatedDists(t, "sum", first, perPodLatency)
	if len(got) != 1 {
		t.Fatalf("got %d families, want one: %v", len(got), got)
	}
	dist := got[0].dist
	total := distScrapeCount(dist)
	if total != 2 {
		t.Fatalf("folded family spans %d scrapes, want 2", total)
	}
	// The older column holds pod a alone; the newer holds both.
	inf := dist.findPoint(math.Inf(+1))
	if inf == nil {
		t.Fatalf("folded family has no +Inf bucket: %v", dist.Points)
	}
	if got := valueAt(inf, 0, total); got != 4 {
		t.Errorf("older +Inf = %v, want pod a alone", got)
	}
	if got := valueAt(inf, 1, total); got != 14 {
		t.Errorf("newest +Inf = %v, want 14", got)
	}
	// pod b's le=1 bucket was not reported at the older scrape, which reads as a
	// gap rather than as a count of zero.
	if got := valueAt(dist.Points[0].Series, 0, total); !math.IsNaN(got) {
		t.Errorf("older le=1 = %v, want a gap: no member reported that bound yet", got)
	}
}

// hide-static has to judge the folded family, not its members: a quiet target
// folded in with a busy one belongs in the total, and dropping it first would
// both understate the total and be a different list from the metrics view's.
func TestHideStaticJudgesTheFoldedDistribution(t *testing.T) {
	const moved = `# TYPE lat_seconds histogram
lat_seconds_bucket{pod="a",le="1"} 2
lat_seconds_bucket{pod="a",le="+Inf"} 4
lat_seconds_sum{pod="a"} 3
lat_seconds_count{pod="a"} 4
lat_seconds_bucket{pod="b",le="1"} 6
lat_seconds_bucket{pod="b",le="+Inf"} 11
lat_seconds_sum{pod="b"} 8
lat_seconds_count{pod="b"} 11
`
	m := distModel(t, 120, perPodLatency, moved)
	m.cfg.HideStatic = true
	if got := len(m.visibleDistributions()); got != 1 {
		t.Fatalf("got %d families, want only the pod that moved", got)
	}

	m.cfg.Aggregation = "sum"
	got := m.visibleDistributions()
	if len(got) != 1 {
		t.Fatalf("the folded family moves 14 -> 15 and should have survived: %v", got)
	}
	if buckets := bucketValues(got[0].dist); buckets["+Inf"] != 15 {
		t.Errorf("+Inf = %v, want the quiet pod counted too", buckets["+Inf"])
	}
}

// The accordion keys on the group, so the two members that now share a row share
// its expansion state rather than each claiming one the row can never read back.
func TestFoldedFamilyExpandsAsOneRow(t *testing.T) {
	m := distModel(t, 120, perPodLatency)
	m.cfg.Aggregation = "sum"
	m.bucketMode = BucketModeCumulative
	m.refresh()

	m.expandStep()
	rows := distRows(t, m)
	if len(rows) < 4 {
		t.Fatalf("got %d lines, want the family line and its grid:\n%v", len(rows), rows)
	}
	if !strings.HasPrefix(plain(rows[0]), "▾") {
		t.Errorf("family line = %q, want the expanded marker", plain(rows[0]))
	}
	// le, then the two folded bounds.
	if !strings.Contains(plain(rows[2]), "8") || !strings.Contains(plain(rows[3]), "14") {
		t.Errorf("grid does not hold the folded counts:\n%v", rows[1:4])
	}
}

// mustRender returns the rendered distribution view.
func mustRender(t *testing.T, m model) string {
	t.Helper()
	content, _ := m.renderDistributions()
	return content
}
