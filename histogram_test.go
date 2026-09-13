package main

import (
	"math"
	"testing"
	"time"
)

// latencyFixture has hand-chosen bucket counts so every quantile below can be
// worked out on paper: 100 observations, 20 under 0.1, 40 more under 0.5, 30
// more under 1 and the last 10 above it.
const latencyFixture = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 20
lat_seconds_bucket{le="0.5"} 60
lat_seconds_bucket{le="1"} 90
lat_seconds_bucket{le="+Inf"} 100
lat_seconds_sum 45
lat_seconds_count 100
`

const summaryFixture = `# TYPE rpc_seconds summary
rpc_seconds{quantile="0.5"} 0.031
rpc_seconds{quantile="0.9"} 0.24
rpc_seconds{quantile="0.99"} 0.51
rpc_seconds_sum 12.4
rpc_seconds_count 400
`

// distFromText ingests one or more scrapes of the same fixture text and returns
// the single distribution they describe.
func distFromText(t *testing.T, sig string, texts ...string) *DistributionSeries {
	t.Helper()
	store := NewStore(10)
	for _, text := range texts {
		store.UpdateFromFamilies(parseFamilies(t, text))
	}
	dist := store.Distributions[sig]
	if dist == nil {
		t.Fatalf("distribution %s not found, have %v", sig, keysOf(store.Distributions))
	}
	return dist
}

func TestEstimateQuantileInterpolatesWithinTheContainingBucket(t *testing.T) {
	dist := distFromText(t, `lat_seconds{}`, latencyFixture)

	// rank 50 falls in (0.1, 0.5], which holds 40 observations. 30 of them are
	// below the rank, so the estimate sits three quarters across the band.
	got := estimateQuantile(dist, 0, 0.5)
	if !got.OK || got.Beyond {
		t.Fatalf("p50 = %+v, want an interpolated estimate", got)
	}
	if math.Abs(got.Value-0.4) > 1e-9 {
		t.Errorf("p50 = %v, want 0.4", got.Value)
	}

	// rank 90 lands exactly on the top of (0.5, 1].
	if got := estimateQuantile(dist, 0, 0.9); !got.OK || got.Beyond || math.Abs(got.Value-1.0) > 1e-9 {
		t.Errorf("p90 = %+v, want 1.0", got)
	}
}

func TestEstimateQuantileInterpolatesFirstBucketFromZero(t *testing.T) {
	dist := distFromText(t, `lat_seconds{}`, latencyFixture)

	// rank 10 falls in the lowest bucket, which has no bound below it. The band
	// must be read as (0, 0.1] rather than as having no width.
	got := estimateQuantile(dist, 0, 0.1)
	if !got.OK || got.Beyond {
		t.Fatalf("p10 = %+v, want an interpolated estimate", got)
	}
	if math.Abs(got.Value-0.05) > 1e-9 {
		t.Errorf("p10 = %v, want 0.05", got.Value)
	}
}

func TestEstimateQuantileInInfBucketReportsALowerBound(t *testing.T) {
	dist := distFromText(t, `lat_seconds{}`, latencyFixture)

	// rank 99 falls in (1, +Inf], where there is nothing to interpolate towards.
	got := estimateQuantile(dist, 0, 0.99)
	if !got.OK {
		t.Fatalf("p99 = %+v, want a result", got)
	}
	if !got.Beyond {
		t.Error("p99 landed in +Inf and must be reported as a lower bound only")
	}
	if got.Value != 1 {
		t.Errorf("p99 lower bound = %v, want the last finite bound 1", got.Value)
	}
}

func TestEstimateQuantileGivesUpWithoutObservations(t *testing.T) {
	const empty = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 0
lat_seconds_bucket{le="+Inf"} 0
lat_seconds_count 0
`
	if got := estimateQuantile(distFromText(t, `lat_seconds{}`, empty), 0, 0.5); got.OK {
		t.Errorf("zero observations produced an estimate %+v", got)
	}

	// A scrape where the family vanished is all NaN and carries no information.
	store := NewStore(10)
	store.UpdateFromFamilies(parseFamilies(t, latencyFixture))
	store.UpdateFromFamilies(parseFamilies(t, "# TYPE other gauge\nother 1\n"))
	dist := store.Distributions[`lat_seconds{}`]
	if got := estimateQuantile(dist, 1, 0.5); got.OK {
		t.Errorf("NaN scrape produced an estimate %+v", got)
	}
	// The scrape before it is still readable.
	if got := estimateQuantile(dist, 0, 0.5); !got.OK {
		t.Error("earlier scrape should still yield an estimate")
	}
}

// A broken exporter can report a bucket smaller than the one below it. Such a
// bucket must be read as holding the running maximum, since a cumulative count
// cannot really shrink; taking it at face value inflates the band beneath the
// answer and drags the estimate towards the top of its bucket.
func TestEstimateQuantileClampsNonMonotonicBuckets(t *testing.T) {
	const broken = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 49
lat_seconds_bucket{le="0.5"} 1
lat_seconds_bucket{le="1"} 51
lat_seconds_bucket{le="+Inf"} 100
`
	// rank 50 falls in (0.5, 1]. Clamping the dipped bucket to 49 puts the rank
	// halfway across the band's two observations, at 0.75. Trusting the reported 1
	// would instead span 49 observations and land at 0.99.
	got := estimateQuantile(distFromText(t, `lat_seconds{}`, broken), 0, 0.5)
	if !got.OK || got.Beyond {
		t.Fatalf("p50 = %+v, want a clamped estimate", got)
	}
	if math.Abs(got.Value-0.75) > 1e-9 {
		t.Errorf("p50 = %v, want 0.75", got.Value)
	}
}

func TestSummaryQuantilesAreReadNotEstimated(t *testing.T) {
	dist := distFromText(t, `rpc_seconds{}`, summaryFixture)

	// Interpolation would never land exactly on the reported value.
	if got := estimateQuantile(dist, 0, 0.5); !got.OK || got.Value != 0.031 {
		t.Errorf("p50 = %+v, want the reported 0.031", got)
	}
	if got := estimateQuantile(dist, 0, 0.99); !got.OK || got.Value != 0.51 {
		t.Errorf("p99 = %+v, want the reported 0.51", got)
	}
	// A quantile the exporter does not publish cannot be derived from the others.
	if got := estimateQuantile(dist, 0, 0.95); got.OK {
		t.Errorf("p95 = %+v, want no estimate for an unpublished quantile", got)
	}
}

func TestBucketModesTransformOneCumulativeFixture(t *testing.T) {
	const second = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 25
lat_seconds_bucket{le="0.5"} 70
lat_seconds_bucket{le="1"} 105
lat_seconds_bucket{le="+Inf"} 120
lat_seconds_sum 52
lat_seconds_count 120
`
	dist := distFromText(t, `lat_seconds{}`, latencyFixture, second)

	cumulative := bucketDisplayValues(dist, BucketModeCumulative, DeltaModeOff)
	wantCumulative := [][]float64{{20, 25}, {60, 70}, {90, 105}, {100, 120}}
	assertRows(t, "cumulative", cumulative, wantCumulative)

	// Each band holds only its own observations; +Inf holds those above 1.
	perBucket := bucketDisplayValues(dist, BucketModePerBucket, DeltaModeOff)
	wantPerBucket := [][]float64{{20, 25}, {40, 45}, {30, 35}, {10, 15}}
	assertRows(t, "per-bucket", perBucket, wantPerBucket)

	// Every band gained 5 observations between the two scrapes. The delta mode
	// puts that 5 in the column it was earned from and leaves the newest column
	// absolute, exactly as it does for a scalar metric.
	perBucketDelta := bucketDisplayValues(dist, BucketModePerBucket, DeltaModeNext)
	wantPerBucketDelta := [][]float64{{5, 25}, {5, 45}, {5, 35}, {5, 15}}
	assertRows(t, "per-bucket with deltas", perBucketDelta, wantPerBucketDelta)

	// The same time transform over the bounds the exporter published.
	cumulativeDelta := bucketDisplayValues(dist, BucketModeCumulative, DeltaModeNext)
	wantCumulativeDelta := [][]float64{{5, 25}, {10, 70}, {15, 105}, {20, 120}}
	assertRows(t, "cumulative with deltas", cumulativeDelta, wantCumulativeDelta)
}

// The bucket mode collapses a scrape down its bounds and the delta mode walks a
// row across time. They are independent axes, so the delta mode has to reach the
// grid whichever bucket mode is showing - it used to be discarded in one of them.
func TestDeltaModeAppliesInEveryBucketMode(t *testing.T) {
	const second = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 25
lat_seconds_bucket{le="0.5"} 70
lat_seconds_bucket{le="1"} 105
lat_seconds_bucket{le="+Inf"} 140
`
	dist := distFromText(t, `lat_seconds{}`, latencyFixture, second)
	for _, mode := range []BucketMode{BucketModeCumulative, BucketModePerBucket} {
		off := bucketDisplayValues(dist, mode, DeltaModeOff)
		on := bucketDisplayValues(dist, mode, DeltaModeNext)
		if off[0][0] == on[0][0] {
			t.Errorf("%v: the oldest column is %v with and without deltas, want the delta mode to reach it",
				mode, off[0][0])
		}
	}
}

func TestCounterResetIsNotRenderedAsANegativeDelta(t *testing.T) {
	const restarted = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 5
lat_seconds_bucket{le="0.5"} 10
lat_seconds_bucket{le="1"} 12
lat_seconds_bucket{le="+Inf"} 15
lat_seconds_count 15
`
	dist := distFromText(t, `lat_seconds{}`, latencyFixture, restarted)
	// The delta belongs to the column it was earned from, which is the one before
	// the restart. Bucket counts are counters, so the drop is blanked rather than
	// rendered as a large negative number.
	rows := bucketDisplayValues(dist, BucketModePerBucket, DeltaModeNext)
	for i, row := range rows {
		if got := row[len(row)-2]; !math.IsNaN(got) {
			t.Errorf("bucket %d delta across a reset = %v, want no value", i, got)
		}
	}
}

// A bucket the exporter only starts reporting mid-run has a shorter history. The
// column it is missing from must read as missing, not shift its values left.
func TestBucketAppearingMidRunStaysRightAligned(t *testing.T) {
	const without = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 10
lat_seconds_bucket{le="+Inf"} 20
`
	const with = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 11
lat_seconds_bucket{le="0.5"} 15
lat_seconds_bucket{le="+Inf"} 22
`
	dist := distFromText(t, `lat_seconds{}`, without, with)
	rows := bucketDisplayValues(dist, BucketModeCumulative, DeltaModeOff)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	newBucket := rows[1]
	if len(newBucket) != 2 {
		t.Fatalf("new bucket row = %v, want padding to the full scrape count", newBucket)
	}
	if !math.IsNaN(newBucket[0]) || newBucket[1] != 15 {
		t.Errorf("new bucket row = %v, want [NaN 15]", newBucket)
	}

	// Decumulation must not treat the absent bucket as a baseline of zero.
	perBucket := bucketDisplayValues(dist, BucketModePerBucket, DeltaModeOff)
	if got := perBucket[2][0]; got != 10 {
		t.Errorf("+Inf band at the first scrape = %v, want 20-10=10", got)
	}
}

func TestDistSummaryDerivesCountAndRate(t *testing.T) {
	const second = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 25
lat_seconds_bucket{le="+Inf"} 120
lat_seconds_count 120
`
	dist := distFromText(t, `lat_seconds{}`, latencyFixture, second)
	stats := distSummary(dist, 1, 5*time.Second)

	if !stats.CountOK || stats.Count != 120 {
		t.Errorf("count = %v (ok=%v), want 120", stats.Count, stats.CountOK)
	}
	if !stats.RateOK || math.Abs(stats.Rate-4) > 1e-9 {
		t.Errorf("rate = %v (ok=%v), want 20 observations over 5s", stats.Rate, stats.RateOK)
	}
	if len(stats.Cells) != len(displayQuantiles) {
		t.Errorf("got %d quantile cells, want %d", len(stats.Cells), len(displayQuantiles))
	}
}

func TestDistSummaryReportsNoRateAcrossAReset(t *testing.T) {
	const restarted = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="+Inf"} 15
lat_seconds_count 15
`
	dist := distFromText(t, `lat_seconds{}`, latencyFixture, restarted)
	if stats := distSummary(dist, 1, 5*time.Second); stats.RateOK {
		t.Errorf("rate = %v across a counter reset, want none", stats.Rate)
	}
}

// A scrape the family was absent for widens the rate window rather than voiding
// it: 40 observations over two intervals is still 4 per second.
func TestDistSummaryWidensTheRateWindowOverAHole(t *testing.T) {
	const third = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="+Inf"} 140
lat_seconds_count 140
`
	store := NewStore(10)
	store.UpdateFromFamilies(parseFamilies(t, latencyFixture))
	store.UpdateFromFamilies(parseFamilies(t, "# TYPE other gauge\nother 1\n"))
	store.UpdateFromFamilies(parseFamilies(t, third))

	stats := distSummary(store.Distributions[`lat_seconds{}`], 2, 5*time.Second)
	if !stats.RateOK || math.Abs(stats.Rate-4) > 1e-9 {
		t.Errorf("rate = %v (ok=%v), want 40 observations over 10s", stats.Rate, stats.RateOK)
	}
}

func TestDistributionIsStaticOnlyWhenNothingMoves(t *testing.T) {
	still := distFromText(t, `lat_seconds{}`, latencyFixture, latencyFixture)
	if !still.IsStatic() {
		t.Error("an unchanging histogram should be static")
	}

	const moved = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 20
lat_seconds_bucket{le="0.5"} 60
lat_seconds_bucket{le="1"} 90
lat_seconds_bucket{le="+Inf"} 101
lat_seconds_sum 45
lat_seconds_count 100
`
	if distFromText(t, `lat_seconds{}`, latencyFixture, moved).IsStatic() {
		t.Error("a histogram with one moving bucket is not static")
	}

	// The count alone moving is enough, even with every bucket unchanged.
	const counted = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 20
lat_seconds_bucket{le="0.5"} 60
lat_seconds_bucket{le="1"} 90
lat_seconds_bucket{le="+Inf"} 100
lat_seconds_sum 45
lat_seconds_count 101
`
	if distFromText(t, `lat_seconds{}`, latencyFixture, counted).IsStatic() {
		t.Error("a histogram whose count moves is not static")
	}
}

func TestFormatFloatKeepsSmallValuesVisible(t *testing.T) {
	cases := []struct {
		val  float64
		want string
	}{
		// The bug this replaced: %.2f rounded all of these to "0".
		{0.0034, "0.0034"},
		{0.08, "0.08"},
		{0.0001234, "0.000123"},
		// Integers stay exact rather than being abbreviated or rounded.
		{21203, "21203"},
		{0, "0"},
		{-7, "-7"},
		// Fractions carry three significant digits without losing integer digits.
		{1.23456, "1.23"},
		{12.3456, "12.3"},
		{123.456, "123"},
		{1234.56, "1235"},
		{-0.0034, "-0.0034"},
	}
	for _, tc := range cases {
		if got := formatFloat(tc.val); got != tc.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tc.val, got, tc.want)
		}
	}
}

// The delta rendering in the metrics table keys off the exact strings "0" and
// "-0" to blank an unchanged value out.
func TestFormatFloatKeepsZeroSpelledTheSameWay(t *testing.T) {
	if got := formatFloat(0); got != "0" {
		t.Errorf("formatFloat(0) = %q, want %q", got, "0")
	}
	if got := formatFloat(math.Copysign(0, -1)); got != "-0" {
		t.Errorf("formatFloat(-0) = %q, want %q", got, "-0")
	}
}

func TestFormatCompactAbbreviatesOnlyLargeValues(t *testing.T) {
	cases := []struct {
		val  float64
		want string
	}{
		{892, "892"},
		{0.5, "0.5"},
		{1000, "1k"},
		{12400, "12.4k"},
		{124000, "124k"},
		{1500000, "1.5M"},
		{2.5e9, "2.5G"},
		{-12400, "-12.4k"},
	}
	for _, tc := range cases {
		if got := formatCompact(tc.val); got != tc.want {
			t.Errorf("formatCompact(%v) = %q, want %q", tc.val, got, tc.want)
		}
	}
}

func TestMatchesFiltersAppliesTheSameRulesToBothViews(t *testing.T) {
	labels := map[string]string{"handler": "/api", "env": "prod"}

	cases := []struct {
		name        string
		metric      string
		label       string
		seriesName  string
		wantMatched bool
	}{
		{"no filters admits everything", "", "", "anything", true},
		{"metric regex matches", "^lat_", "", "lat_seconds", true},
		{"metric regex rejects", "^lat_", "", "rpc_seconds", false},
		{"exact label matches", "", "env=prod", "lat_seconds", true},
		{"exact label mismatch", "", "env=dev", "lat_seconds", false},
		{"absent label key rejects", "", "zone=eu", "lat_seconds", false},
		{"label regex matches", "", "handler=~^/api", "lat_seconds", true},
		{"label regex rejects", "", "handler=~^/ui", "lat_seconds", false},
		{"bare regex matches any value", "", "^/api$", "lat_seconds", true},
		{"bare regex matching nothing rejects", "", "^/nope$", "lat_seconds", false},
		{"both filters must pass", "^lat_", "env=dev", "lat_seconds", false},
		{"every clause must pass", "", "env=prod,handler=/api", "lat_seconds", true},
		{"one failing clause rejects", "", "env=prod,handler=/ui", "lat_seconds", false},
		{"a negated clause holds", "", "env=prod,handler!=/ui", "lat_seconds", true},
		{"a negated clause rejects", "", "env!=prod", "lat_seconds", false},
		{"a negated regex", "", "handler!~^/ui,env=~pro", "lat_seconds", true},
		{"an absent label satisfies a negation", "", "zone!=eu", "lat_seconds", true},
		{"an absent label fails a positive clause", "", "zone=eu,env=prod", "lat_seconds", false},
		{"a keyed and a bare clause", "", "env=prod,^/api$", "lat_seconds", true},
		{"a comma inside a repetition is not a separator", "", "env=~pro{1,2}d", "lat_seconds", true},
		{"the metric filter still applies alongside clauses", "^rpc_", "env=prod,handler=/api", "lat_seconds", false},
	}
	for _, tc := range cases {
		m := model{cfg: Config{FilterMetric: tc.metric, FilterLabel: tc.label}}
		if got := m.matchesFilters(tc.seriesName, labels); got != tc.wantMatched {
			t.Errorf("%s: matchesFilters = %v, want %v", tc.name, got, tc.wantMatched)
		}
	}
}

// assertRows compares value grids treating NaN as a value in its own right, since
// NaN marks "not reported at this scrape" throughout the store.
func assertRows(t *testing.T, label string, got, want [][]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d rows, want %d", label, len(got), len(want))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Errorf("%s: row %d = %v, want %v", label, i, got[i], want[i])
			continue
		}
		for j := range want[i] {
			g, w := got[i][j], want[i][j]
			if math.IsNaN(g) && math.IsNaN(w) {
				continue
			}
			if g != w {
				t.Errorf("%s: row %d = %v, want %v", label, i, got[i], want[i])
				break
			}
		}
	}
}
