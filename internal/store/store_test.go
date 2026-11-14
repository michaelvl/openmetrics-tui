package store

import (
	"math"
	"testing"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/testutil"
)

func TestDistributionsAreKeptOutOfSimpleMetrics(t *testing.T) {
	store := NewStore(10)
	store.UpdateFromFamilies(testutil.ParseFamilies(t, testutil.HistogramFixture), time.Time{})

	var names []string
	for _, series := range store.Metrics {
		names = append(names, series.Name)
	}
	if len(store.Metrics) != 2 {
		t.Fatalf("Metrics should hold only the gauge and the counter, got %v", names)
	}
	if got := store.Metrics[`requests_in_flight{}`]; got == nil || got.Kind != KindGauge {
		t.Errorf("requests_in_flight should be a gauge, got %+v", got)
	}
	if got := store.Metrics[`requests_total{}`]; got == nil || got.Kind != KindCounter {
		t.Errorf("requests_total should be a counter, got %+v", got)
	}

	if len(store.Distributions) != 1 {
		t.Fatalf("expected 1 distribution, got %d", len(store.Distributions))
	}
	dist := store.Distributions[`request_duration_seconds{method="GET"}`]
	if dist == nil {
		t.Fatalf("histogram not found, have %v", testutil.KeysOf(store.Distributions))
	}
	if dist.Kind != KindHistogram {
		t.Errorf("Kind = %v, want histogram", dist.Kind)
	}
	if _, ok := dist.Labels["le"]; ok {
		t.Errorf("family labels should not carry le, got %v", dist.Labels)
	}
	if dist.Labels["method"] != "GET" {
		t.Errorf("family labels = %v, want method=GET", dist.Labels)
	}
}

func TestBucketsAreSortedAndNamedLikeTheWireFormat(t *testing.T) {
	store := NewStore(10)
	store.UpdateFromFamilies(testutil.ParseFamilies(t, testutil.HistogramFixture), time.Time{})
	dist := store.Distributions[`request_duration_seconds{method="GET"}`]

	wantBounds := []float64{0.05, 1, math.Inf(+1)}
	if len(dist.Points) != len(wantBounds) {
		t.Fatalf("got %d points, want %d", len(dist.Points), len(wantBounds))
	}
	wantLabels := []string{"0.05", "1", "+Inf"}
	for i, point := range dist.Points {
		if point.Bound != wantBounds[i] {
			t.Errorf("point %d bound = %v, want %v", i, point.Bound, wantBounds[i])
		}
		if point.Series.Name != "request_duration_seconds_bucket" {
			t.Errorf("point %d name = %q", i, point.Series.Name)
		}
		if got := point.Series.Labels["le"]; got != wantLabels[i] {
			t.Errorf("point %d le = %q, want %q", i, got, wantLabels[i])
		}
		if point.Series.Labels["method"] != "GET" {
			t.Errorf("point %d lost the family labels: %v", i, point.Series.Labels)
		}
		if point.Series.Kind != KindCounter {
			t.Errorf("point %d kind = %v, want counter", i, point.Series.Kind)
		}
	}

	if dist.Sum == nil || dist.Sum.Name != "request_duration_seconds_sum" || dist.Sum.Values[0] != 612.44 {
		t.Errorf("sum series = %+v", dist.Sum)
	}
	if dist.Count == nil || dist.Count.Name != "request_duration_seconds_count" || dist.Count.Values[0] != 5000 {
		t.Errorf("count series = %+v", dist.Count)
	}
	// Buckets are stored exactly as exposed: cumulative and raw.
	if got := dist.Points[1].Series.Values[0]; got != 4991 {
		t.Errorf("bucket le=1 = %v, want the cumulative 4991", got)
	}
}

// Every series must gain exactly one value per scrape, so that an index into
// Values always refers to the same scrape no matter which series it came from.
func TestScrapesStayAlignedWhenAFamilyDisappears(t *testing.T) {
	store := NewStore(10)
	store.UpdateFromFamilies(testutil.ParseFamilies(t, testutil.HistogramFixture), time.Time{})
	store.UpdateFromFamilies(testutil.ParseFamilies(t, testutil.HistogramFixture), time.Time{})
	store.UpdateFromFamilies(testutil.ParseFamilies(t, "# TYPE requests_in_flight gauge\nrequests_in_flight 9\n"), time.Time{})

	for sig, series := range store.Metrics {
		if len(series.Values) != 3 {
			t.Errorf("%s has %d values, want 3", sig, len(series.Values))
		}
	}
	if got := store.Metrics[`requests_total{}`].Values; !math.IsNaN(got[2]) {
		t.Errorf("vanished counter should end in NaN, got %v", got)
	}

	dist := store.Distributions[`request_duration_seconds{method="GET"}`]
	dist.EachSeries("sig", func(key string, series *MetricSeries) {
		if len(series.Values) != 3 {
			t.Errorf("%s has %d values, want 3", key, len(series.Values))
			return
		}
		if !math.IsNaN(series.Values[2]) {
			t.Errorf("%s should end in NaN once the family disappears, got %v", key, series.Values)
		}
	})
	if len(dist.Points) != 3 {
		t.Errorf("a vanished family should keep its buckets, got %d", len(dist.Points))
	}
}

func TestSummaryQuantilesAreGauges(t *testing.T) {
	const fixture = `# TYPE rpc_duration_seconds summary
rpc_duration_seconds{quantile="0.5"} 0.12
rpc_duration_seconds{quantile="0.99"} 0.42
rpc_duration_seconds_count 12
`
	store := NewStore(10)
	store.UpdateFromFamilies(testutil.ParseFamilies(t, fixture), time.Time{})

	dist := store.Distributions[`rpc_duration_seconds{}`]
	if dist == nil {
		t.Fatalf("summary not found, have %v", testutil.KeysOf(store.Distributions))
	}
	if dist.Kind != KindSummary {
		t.Errorf("Kind = %v, want summary", dist.Kind)
	}
	if len(dist.Points) != 2 {
		t.Fatalf("got %d quantiles, want 2", len(dist.Points))
	}
	for _, point := range dist.Points {
		// Quantiles keep the family name, exactly as the text format writes them.
		if point.Series.Name != "rpc_duration_seconds" {
			t.Errorf("quantile name = %q", point.Series.Name)
		}
		if point.Series.Kind != KindGauge {
			t.Errorf("quantile kind = %v, want gauge (values, not counters)", point.Series.Kind)
		}
	}
	if got := dist.Points[1].Series.Labels["quantile"]; got != "0.99" {
		t.Errorf("quantile label = %q, want 0.99", got)
	}
	if len(store.Metrics) != 0 {
		t.Errorf("summary leaked into the simple metrics: %v", testutil.KeysOf(store.Metrics))
	}
}

// An absent sum must stay absent rather than becoming a series pinned at zero,
// which is what the generated getters would report.
func TestMissingSumStaysNil(t *testing.T) {
	const fixture = `# TYPE rpc_duration_seconds summary
rpc_duration_seconds{quantile="0.5"} 0.12
rpc_duration_seconds_count 12
`
	store := NewStore(10)
	store.UpdateFromFamilies(testutil.ParseFamilies(t, fixture), time.Time{})

	dist := store.Distributions[`rpc_duration_seconds{}`]
	if dist.Sum != nil {
		t.Errorf("Sum = %+v, want nil when the exporter omits _sum", dist.Sum)
	}
	if dist.Count == nil || dist.Count.Values[0] != 12 {
		t.Errorf("Count = %+v, want the reported 12", dist.Count)
	}
}

func TestHistoryLimitPrunesBuckets(t *testing.T) {
	store := NewStore(2)
	for i := 0; i < 4; i++ {
		store.UpdateFromFamilies(testutil.ParseFamilies(t, testutil.HistogramFixture), time.Time{})
	}
	dist := store.Distributions[`request_duration_seconds{method="GET"}`]
	for _, point := range dist.Points {
		if len(point.Series.Values) != 2 {
			t.Errorf("bucket le=%v kept %d values, want the limit of 2", point.Bound, len(point.Series.Values))
		}
	}
	if len(dist.Sum.Values) != 2 {
		t.Errorf("sum kept %d values, want 2", len(dist.Sum.Values))
	}
}

func TestFormatBound(t *testing.T) {
	cases := map[float64]string{
		0.05:         "0.05",
		1:            "1",
		math.Inf(+1): "+Inf",
		2.5e-07:      "2.5e-07",
	}
	for bound, want := range cases {
		if got := FormatBound(bound); got != want {
			t.Errorf("FormatBound(%v) = %q, want %q", bound, got, want)
		}
	}
}

// ValuesWithDeltas is the display's time axis, shared by the metrics table and
// by every histogram bucket row, so its three modes are pinned here rather than
// only through whichever view happens to call it.
func TestValuesWithDeltasTransformsEachMode(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name   string
		kind   MetricKind
		values []float64
		mode   string
		want   []float64
	}{
		{"off returns the values untouched", KindCounter, []float64{10, 14, 20}, DeltaModeOff, []float64{10, 14, 20}},
		// The delta sits in the column it was earned from, and the newest column
		// stays absolute - that is what makes "next" readable beside a raw table.
		{"next is a forward difference", KindCounter, []float64{10, 14, 20}, DeltaModeNext, []float64{4, 6, 20}},
		// "view" agrees with "next" on the history and spends the newest column on
		// the growth across everything on screen - the sum of the deltas beside it,
		// newest sample included, so the column never lags a scrape behind its row.
		{"view spans the history", KindCounter, []float64{10, 14, 20}, DeltaModeView, []float64{4, 6, 10}},
		{"view spans a two-sample window", KindCounter, []float64{10, 20}, DeltaModeView, []float64{10, 10}},
		{"view needs two samples", KindCounter, []float64{10}, DeltaModeView, []float64{nan}},
		{"view spans across a gap", KindCounter, []float64{10, nan, 20}, DeltaModeView, []float64{nan, nan, 10}},
		{"a gap blanks the deltas that touch it", KindCounter, []float64{10, nan, 20}, DeltaModeNext, []float64{nan, nan, 20}},
		// A counter only falls when the process restarted, so the drop is not a
		// measurement and must not be rendered as one.
		{"a counter reset is blanked", KindCounter, []float64{100, 5, 9}, DeltaModeNext, []float64{nan, 4, 9}},
		{"a reset inside the window blanks the view span", KindCounter, []float64{100, 5, 9}, DeltaModeView, []float64{nan, 4, nan}},
		// The span ends above where it started, so only a reset found between the
		// samples themselves can blank it.
		{"a reset the endpoints hide still blanks the span", KindCounter, []float64{100, 5, 200}, DeltaModeView, []float64{nan, 195, nan}},
		// A gauge is free to fall; blanking that would hide the measurement.
		{"a falling gauge keeps its negative delta", KindGauge, []float64{100, 5, 9}, DeltaModeNext, []float64{-95, 4, 9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (&MetricSeries{Kind: tc.kind, Values: tc.values}).ValuesWithDeltas(tc.mode, nil)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if math.IsNaN(got[i]) && math.IsNaN(tc.want[i]) {
					continue
				}
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A failed scrape splits the history into epochs: the target most likely
// restarted, so the columns either side of it come from different processes.
func TestFailedScrapesStartANewEpoch(t *testing.T) {
	const fixture = "# TYPE g gauge\ng 1\n"
	t0 := time.Unix(1000, 0)
	store := NewStore(10)

	// A failure before anything was scraped separates nothing.
	store.MarkBreak()
	store.UpdateFromFamilies(testutil.ParseFamilies(t, fixture), t0)
	// Consecutive failures are one outage, so one break.
	store.MarkBreak()
	store.MarkBreak()
	store.UpdateFromFamilies(testutil.ParseFamilies(t, fixture), t0.Add(30*time.Second))
	store.UpdateFromFamilies(testutil.ParseFamilies(t, fixture), t0.Add(35*time.Second))

	if got, want := store.Epochs(3), []int{0, 1, 1}; !equalInts(got, want) {
		t.Errorf("epochs = %v, want %v", got, want)
	}
	if got := store.EpochParity(0); got != 0 {
		t.Errorf("newest column parity = %d, want 0", got)
	}
	if got := store.EpochParity(2); got != 1 {
		t.Errorf("pre-break column parity = %d, want 1", got)
	}
	if scrape, _ := store.ScrapeAt(1); !scrape.Time.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("ScrapeAt(1).Time = %v, want the second scrape's time", scrape.Time)
	}
}

// The scrape log is pruned with the series, so the two stay right-aligned.
func TestScrapeLogIsPrunedWithTheHistory(t *testing.T) {
	store := NewStore(2)
	for i := 0; i < 5; i++ {
		store.UpdateFromFamilies(testutil.ParseFamilies(t, "# TYPE g gauge\ng 1\n"), time.Unix(int64(i), 0))
	}
	if len(store.Scrapes) != 2 || len(store.Metrics[`g{}`].Values) != 2 {
		t.Fatalf("scrapes = %d, values = %d, want 2 each", len(store.Scrapes), len(store.Metrics[`g{}`].Values))
	}
	if got := store.Scrapes[1].Time; !got.Equal(time.Unix(4, 0)) {
		t.Errorf("newest scrape time = %v, want the last scrape", got)
	}
}

// Deltas never span an epoch boundary, whatever the kind: across a restart even
// a rising counter or a falling gauge mixes two processes' numbers.
func TestValuesWithDeltasStopAtAnEpochBoundary(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name   string
		kind   MetricKind
		values []float64
		epochs []int
		mode   string
		want   []float64
	}{
		{"a rising counter across a break is blanked", KindCounter, []float64{10, 14, 20}, []int{0, 1, 1}, DeltaModeNext, []float64{nan, 6, 20}},
		{"a gauge across a break is blanked", KindGauge, []float64{100, 5, 9}, []int{0, 1, 1}, DeltaModeNext, []float64{nan, 4, 9}},
		{"view spans only the newest epoch", KindCounter, []float64{10, 14, 20, 26}, []int{0, 0, 1, 1}, DeltaModeView, []float64{4, nan, 6, 6}},
		{"view needs two samples in the newest epoch", KindCounter, []float64{10, 14, 20}, []int{0, 0, 1}, DeltaModeView, []float64{4, nan, nan}},
		// A series that appeared after the break has a shorter history; it lines
		// up with the tail of the epochs, not the head.
		{"epochs are right-aligned to a shorter series", KindCounter, []float64{14, 20}, []int{0, 1, 1}, DeltaModeNext, []float64{6, 20}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (&MetricSeries{Kind: tc.kind, Values: tc.values}).ValuesWithDeltas(tc.mode, tc.epochs)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if math.IsNaN(got[i]) && math.IsNaN(tc.want[i]) {
					continue
				}
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
