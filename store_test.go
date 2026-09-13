package main

import (
	"math"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	promModel "github.com/prometheus/common/model"
)

// parseFamilies turns an exposition text fixture into the families the fetcher
// hands to the store.
func parseFamilies(t *testing.T, text string) map[string]*dto.MetricFamily {
	t.Helper()
	parser := expfmt.NewTextParser(promModel.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return families
}

const histogramFixture = `# TYPE requests_in_flight gauge
requests_in_flight 7
# TYPE requests_total counter
requests_total 1500
# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{method="GET",le="0.05"} 1203
request_duration_seconds_bucket{method="GET",le="1"} 4991
request_duration_seconds_bucket{method="GET",le="+Inf"} 5000
request_duration_seconds_sum{method="GET"} 612.44
request_duration_seconds_count{method="GET"} 5000
`

func TestDistributionsAreKeptOutOfSimpleMetrics(t *testing.T) {
	store := NewStore(10)
	store.UpdateFromFamilies(parseFamilies(t, histogramFixture))

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
		t.Fatalf("histogram not found, have %v", keysOf(store.Distributions))
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
	store.UpdateFromFamilies(parseFamilies(t, histogramFixture))
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
	store.UpdateFromFamilies(parseFamilies(t, histogramFixture))
	store.UpdateFromFamilies(parseFamilies(t, histogramFixture))
	store.UpdateFromFamilies(parseFamilies(t, "# TYPE requests_in_flight gauge\nrequests_in_flight 9\n"))

	for sig, series := range store.Metrics {
		if len(series.Values) != 3 {
			t.Errorf("%s has %d values, want 3", sig, len(series.Values))
		}
	}
	if got := store.Metrics[`requests_total{}`].Values; !math.IsNaN(got[2]) {
		t.Errorf("vanished counter should end in NaN, got %v", got)
	}

	dist := store.Distributions[`request_duration_seconds{method="GET"}`]
	dist.eachSeries("sig", func(key string, series *MetricSeries) {
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

// A bucket the exporter starts reporting mid-run has a shorter history. The table
// renders values right-aligned, so it still lines up under the current column.
func TestBucketAppearingMidRunIsRightAligned(t *testing.T) {
	const withoutBucket = `# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{le="0.05"} 10
request_duration_seconds_bucket{le="+Inf"} 20
`
	const withBucket = `# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{le="0.05"} 11
request_duration_seconds_bucket{le="0.5"} 15
request_duration_seconds_bucket{le="+Inf"} 22
`
	store := NewStore(10)
	store.UpdateFromFamilies(parseFamilies(t, withoutBucket))
	store.UpdateFromFamilies(parseFamilies(t, withoutBucket))
	store.UpdateFromFamilies(parseFamilies(t, withBucket))

	dist := store.Distributions[`request_duration_seconds{}`]
	if len(dist.Points) != 3 || dist.Points[1].Bound != 0.5 {
		t.Fatalf("new bucket not inserted in sorted position: %v", boundsOf(dist))
	}
	newSeries := dist.Points[1].Series
	if len(newSeries.Values) != 1 {
		t.Fatalf("new bucket should start fresh, got %v", newSeries.Values)
	}

	m := model{cfg: Config{History: 3, DeltaMode: DeltaModeOff, LabelMode: LabelModeHideAll}}
	rows := m.buildTableRows([]*MetricSeries{newSeries, dist.Points[0].Series})
	if got := rows[0]; len(got) != 4 || got[1] != "" || got[2] != "" || got[3] != "15" {
		t.Errorf("new bucket row = %q, want the value in the current column", got)
	}
	if got := rows[1]; len(got) != 4 || got[3] != "11" {
		t.Errorf("established bucket row = %q, want its current value in the same column", got)
	}
}

func TestSummaryQuantilesAreGauges(t *testing.T) {
	const fixture = `# TYPE rpc_duration_seconds summary
rpc_duration_seconds{quantile="0.5"} 0.12
rpc_duration_seconds{quantile="0.99"} 0.42
rpc_duration_seconds_count 12
`
	store := NewStore(10)
	store.UpdateFromFamilies(parseFamilies(t, fixture))

	dist := store.Distributions[`rpc_duration_seconds{}`]
	if dist == nil {
		t.Fatalf("summary not found, have %v", keysOf(store.Distributions))
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
		t.Errorf("summary leaked into the simple metrics: %v", keysOf(store.Metrics))
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
	store.UpdateFromFamilies(parseFamilies(t, fixture))

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
		store.UpdateFromFamilies(parseFamilies(t, histogramFixture))
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
		if got := formatBound(bound); got != want {
			t.Errorf("formatBound(%v) = %q, want %q", bound, got, want)
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
		// the growth across everything else on screen.
		{"view spans the history", KindCounter, []float64{10, 14, 20}, DeltaModeView, []float64{4, 6, 4}},
		{"view needs two historical samples", KindCounter, []float64{10, 20}, DeltaModeView, []float64{10, nan}},
		{"a gap blanks the deltas that touch it", KindCounter, []float64{10, nan, 20}, DeltaModeNext, []float64{nan, nan, 20}},
		// A counter only falls when the process restarted, so the drop is not a
		// measurement and must not be rendered as one.
		{"a counter reset is blanked", KindCounter, []float64{100, 5, 9}, DeltaModeNext, []float64{nan, 4, 9}},
		{"a reset inside the window blanks the view span", KindCounter, []float64{100, 5, 9}, DeltaModeView, []float64{nan, 4, nan}},
		// A gauge is free to fall; blanking that would hide the measurement.
		{"a falling gauge keeps its negative delta", KindGauge, []float64{100, 5, 9}, DeltaModeNext, []float64{-95, 4, 9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (&MetricSeries{Kind: tc.kind, Values: tc.values}).ValuesWithDeltas(tc.mode)
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

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func boundsOf(d *DistributionSeries) []float64 {
	bounds := make([]float64, 0, len(d.Points))
	for _, point := range d.Points {
		bounds = append(bounds, point.Bound)
	}
	return bounds
}
