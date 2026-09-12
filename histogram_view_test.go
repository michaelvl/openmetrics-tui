package main

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	dto "github.com/prometheus/client_model/go"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// withColor forces a colour profile for the duration of one test. lipgloss
// otherwise detects that the test binary is not a terminal and strips every
// style, which would let a shading test pass whether or not anything is shaded.
func withColor(t *testing.T) {
	t.Helper()
	before := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(before) })
}

// backgroundSequence is the background-colour parameter shadeRamp emits under
// that profile. It is matched without the escape prefix because the brightest
// steps set a foreground first.
const backgroundSequence = "48;5;"

// plain strips styling so a test asserts on content rather than on whichever
// colour profile lipgloss picked for the environment it ran in.
func plain(s string) string { return ansiPattern.ReplaceAllString(s, "") }

// distModel builds a model in the distribution view, fed the given scrapes.
func distModel(t *testing.T, width int, texts ...string) model {
	t.Helper()
	store := NewStore(10)
	for _, text := range texts {
		store.UpdateFromFamilies(parseFamilies(t, text))
	}
	m := model{
		cfg: Config{
			Interval:  5 * time.Second,
			History:   10,
			LabelMode: LabelModeShowAll,
			DeltaMode: DeltaModeOff,
		},
		store:         store,
		width:         width,
		height:        24,
		view:          ViewDistributions,
		bucketMode:    BucketModePerBucketDelta,
		expanded:      make(map[string]bool),
		viewport:      viewport.New(width, 20),
		viewportReady: true,
	}
	m.refresh()
	return m
}

// distRows returns the rendered list without its two header lines.
func distRows(t *testing.T, m model) []string {
	t.Helper()
	content, _ := m.renderDistributions()
	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		t.Fatalf("rendered view has no rows:\n%s", content)
	}
	return lines[2:]
}

func TestCollapsedLineSummarisesTheDistribution(t *testing.T) {
	const second = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 25
lat_seconds_bucket{le="0.5"} 70
lat_seconds_bucket{le="1"} 115
lat_seconds_bucket{le="+Inf"} 120
lat_seconds_count 120
`
	m := distModel(t, 120, latencyFixture, second)
	rows := distRows(t, m)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1:\n%v", len(rows), rows)
	}

	got := strings.Fields(plain(rows[0]))
	// marker, name, count, rate, p50, p90, p99. The line reads the latest scrape:
	// 120 observations, 20 of them in the last 5s. p50's rank of 60 falls in
	// (0.1, 0.5], p90's rank of 108 in (0.5, 1], and p99's in +Inf, where there is
	// no upper bound to interpolate towards.
	want := []string{"▸", "lat_seconds", "120", "4/s", "0.411", "0.922", ">1"}
	if len(got) != len(want) {
		t.Fatalf("row = %q, want %d fields", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q (row %q)", i, got[i], want[i], got)
		}
	}
}

// Summaries report their quantiles, so the collapsed line must read them off
// rather than deriving anything.
func TestCollapsedSummaryLineReadsReportedQuantiles(t *testing.T) {
	m := distModel(t, 120, summaryFixture)
	got := strings.Fields(plain(distRows(t, m)[0]))
	if len(got) < 7 {
		t.Fatalf("row = %q, want at least 7 fields", got)
	}
	if got[4] != "0.031" || got[5] != "0.24" || got[6] != "0.51" {
		t.Errorf("quantiles = %q, want the reported 0.031/0.24/0.51", got[4:7])
	}
}

// A native histogram carries only a sum and a count. The collapsed line must
// still work from those rather than rendering a blank row.
func TestCollapsedLineWorksForANativeHistogram(t *testing.T) {
	count, sum := uint64(400), 12.5
	schema, zeroThreshold, zeroCount := int32(0), 0.001, uint64(0)
	store := NewStore(10)
	store.UpdateFromFamilies(map[string]*dto.MetricFamily{
		"native_seconds": {
			Name: strPtr("native_seconds"),
			Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{{
				Histogram: &dto.Histogram{
					SampleCount:   &count,
					SampleSum:     &sum,
					Schema:        &schema,
					ZeroThreshold: &zeroThreshold,
					ZeroCount:     &zeroCount,
					PositiveSpan:  []*dto.BucketSpan{{Offset: int32Ptr(0), Length: uint32Ptr(2)}},
					PositiveDelta: []int64{2, 1},
				},
			}},
		},
	})

	m := distModel(t, 120)
	m.store = store
	m.refresh()

	got := strings.Fields(plain(distRows(t, m)[0]))
	want := []string{"▸", "native_seconds", "400", ".", ".", ".", "."}
	if len(got) != len(want) {
		t.Fatalf("row = %q, want %d fields", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q (row %q)", i, got[i], want[i], got)
		}
	}
}

const threeFamilies = `# TYPE a_seconds histogram
a_seconds_bucket{le="1"} 5
a_seconds_bucket{le="+Inf"} 6
a_seconds_count 6
# TYPE b_seconds histogram
b_seconds_bucket{le="64"} 3
b_seconds_bucket{le="+Inf"} 4
b_seconds_count 4
# TYPE c_seconds summary
c_seconds{quantile="0.5"} 0.2
c_seconds_count 9
`

func TestCursorClampsAtBothEnds(t *testing.T) {
	m := distModel(t, 120, threeFamilies)

	m.moveCursor(-1)
	if m.distCursor != 0 {
		t.Errorf("cursor above the list = %d, want 0", m.distCursor)
	}
	m.moveCursor(10)
	if m.distCursor != 2 {
		t.Errorf("cursor below the list = %d, want the last row 2", m.distCursor)
	}
	m.moveCursor(-1)
	if m.distCursor != 1 {
		t.Errorf("cursor = %d, want 1", m.distCursor)
	}
}

// A filter, the hide-static toggle or the exporter itself can shorten the list
// under a cursor that is already near the end.
func TestCursorFollowsAShrinkingList(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	m.moveCursor(2)
	if m.distCursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.distCursor)
	}

	m.cfg.FilterMetric = "^a_"
	m.refresh()
	if m.distCursor != 0 {
		t.Errorf("cursor = %d after filtering down to one row, want 0", m.distCursor)
	}
}

func TestFiltersApplyToTheDistributionList(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	if got := len(m.visibleDistributions()); got != 3 {
		t.Fatalf("got %d distributions, want 3", got)
	}

	m.cfg.FilterMetric = "_seconds$"
	if got := len(m.visibleDistributions()); got != 3 {
		t.Errorf("got %d distributions, want all 3 to match", got)
	}
	m.cfg.FilterMetric = "^b_"
	if got := len(m.visibleDistributions()); got != 1 {
		t.Errorf("got %d distributions, want 1", got)
	}

	// One scrape is not evidence that anything is unchanging, so hide-static must
	// not drop a family until there are at least two samples to compare.
	m.cfg.FilterMetric = ""
	m.cfg.HideStatic = true
	if got := len(m.visibleDistributions()); got != 3 {
		t.Errorf("got %d distributions, want a single scrape to hide nothing", got)
	}
}

func TestHideStaticDropsOnlyUnchangingFamilies(t *testing.T) {
	const moved = `# TYPE a_seconds histogram
a_seconds_bucket{le="1"} 5
a_seconds_bucket{le="+Inf"} 6
a_seconds_count 6
# TYPE b_seconds histogram
b_seconds_bucket{le="64"} 3
b_seconds_bucket{le="+Inf"} 5
b_seconds_count 5
# TYPE c_seconds summary
c_seconds{quantile="0.5"} 0.2
c_seconds_count 9
`
	m := distModel(t, 120, threeFamilies, moved)
	m.cfg.HideStatic = true

	visible := m.visibleDistributions()
	if len(visible) != 1 {
		t.Fatalf("got %d visible, want only the family that moved: %v", len(visible), visible)
	}
	if visible[0].dist.Name != "b_seconds" {
		t.Errorf("visible = %s, want b_seconds", visible[0].dist.Name)
	}
}

// The value columns are already as narrow as their contents allow, so a long
// label set has to give way rather than push the line past the terminal edge.
func TestLongLabelSetsAreTruncatedToTheTerminalWidth(t *testing.T) {
	const wide = `# TYPE lat_seconds histogram
lat_seconds_bucket{handler="/api/v1/products/search/suggestions",method="GET",le="+Inf"} 4
lat_seconds_count{handler="/api/v1/products/search/suggestions",method="GET"} 4
`
	for _, width := range []int{60, 80, 120} {
		m := distModel(t, width, wide)
		content, _ := m.renderDistributions()
		for _, line := range strings.Split(content, "\n")[1:] {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width %d: line is %d columns wide: %q", width, got, plain(line))
			}
		}
	}
}

func TestBucketModeCyclesBackToWhereItStarted(t *testing.T) {
	mode := BucketModePerBucketDelta
	seen := map[BucketMode]bool{}
	for i := 0; i < 3; i++ {
		seen[mode] = true
		mode = mode.next()
	}
	if len(seen) != 3 {
		t.Errorf("cycling visited %d modes, want all 3", len(seen))
	}
	if mode != BucketModePerBucketDelta {
		t.Errorf("three steps landed on %v, want to be back at the start", mode)
	}
}

// Pressing v twice must land back where it started, in both views.
func TestViewToggleIsARoundTrip(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	m.view = ViewMetrics
	m.store.UpdateFromFamilies(parseFamilies(t, "# TYPE g gauge\ng 1\n"))
	m.refresh()

	m.toggleView()
	if m.view != ViewDistributions {
		t.Fatalf("view = %v, want the distribution view", m.view)
	}
	m.moveCursor(2)
	cursor := m.distCursor

	m.toggleView()
	if m.view != ViewMetrics {
		t.Fatalf("view = %v, want the metrics view", m.view)
	}
	m.toggleView()
	if m.view != ViewDistributions {
		t.Fatalf("view = %v, want the distribution view again", m.view)
	}
	if m.distCursor != cursor {
		t.Errorf("cursor = %d after a round trip, want %d", m.distCursor, cursor)
	}
}

// The distribution view must explain an empty list rather than showing nothing,
// and distinguish "the exporter has none" from "the filters hid them".
func TestEmptyDistributionListExplainsItself(t *testing.T) {
	m := distModel(t, 120, "# TYPE g gauge\ng 1\n")
	content, _ := m.renderDistributions()
	if !strings.Contains(content, "No histograms or summaries") {
		t.Errorf("content = %q, want an explanation", content)
	}
	if strings.Contains(content, "hidden by") {
		t.Errorf("content = %q, should not blame filters when there are none", content)
	}

	m = distModel(t, 120, threeFamilies)
	m.cfg.FilterMetric = "^nothing$"
	content, _ = m.renderDistributions()
	if !strings.Contains(content, "hidden by: metric filter") {
		t.Errorf("content = %q, want the filter named", content)
	}
}

// expandAll opens every visible family, which is the state the shared grid
// layout has to hold up under.
func expandAll(m *model) {
	for _, entry := range m.visibleDistributions() {
		m.expanded[entry.sig] = true
	}
	m.refresh()
}

// gridRows returns the rendered lines belonging to expanded blocks, identified by
// their indent.
func gridRows(t *testing.T, m model) []string {
	t.Helper()
	content, _ := m.renderDistributions()
	var rows []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(plain(line), blockIndent) {
			rows = append(rows, line)
		}
	}
	return rows
}

// Two families with nothing in common but a time axis must still line their
// columns up, because a column that means a different scrape in each block is
// worse than no grid at all.
func TestDisjointBucketLayoutsShareOneTimeAxis(t *testing.T) {
	const first = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 20
lat_seconds_bucket{le="+Inf"} 30
lat_seconds_count 30
# TYPE size_bytes histogram
size_bytes_bucket{le="4096"} 7
size_bytes_bucket{le="65536"} 9
size_bytes_bucket{le="+Inf"} 11
size_bytes_count 11
`
	const second = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 26
lat_seconds_bucket{le="+Inf"} 41
lat_seconds_count 41
# TYPE size_bytes histogram
size_bytes_bucket{le="4096"} 12
size_bytes_bucket{le="65536"} 15
size_bytes_bucket{le="+Inf"} 20
size_bytes_count 20
`
	m := distModel(t, 120, first, second)
	expandAll(&m)

	rows := gridRows(t, m)
	// Two header rows plus 2 and 3 bucket rows.
	if len(rows) != 7 {
		t.Fatalf("got %d grid rows, want 7:\n%s", len(rows), strings.Join(rows, "\n"))
	}

	// Every grid row must put its rightmost column at the same screen offset, or
	// the "Curr" columns do not line up between the two blocks.
	want := lipgloss.Width(rows[0])
	for i, row := range rows {
		if got := lipgloss.Width(row); got != want {
			t.Errorf("grid row %d is %d columns wide, want %d: %q", i, got, want, plain(row))
		}
	}

	// Both bound columns are present and neither block borrowed the other's.
	joined := plain(strings.Join(rows, "\n"))
	for _, bound := range []string{"0.1", "4096", "65536", "+Inf"} {
		if !strings.Contains(joined, bound) {
			t.Errorf("bound %q missing from the grids:\n%s", bound, joined)
		}
	}
}

// A family the exporter stopped reporting leaves a hole. Rendering it as 0 would
// claim the exporter saw no observations, which is a different fact.
func TestMissingScrapesRenderAsGapsNotZeroes(t *testing.T) {
	const present = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 20
lat_seconds_bucket{le="+Inf"} 30
lat_seconds_count 30
`
	const gone = `# TYPE other_total counter
other_total 1
`
	m := distModel(t, 120, present, gone, gone)
	m.bucketMode = BucketModeCumulative
	expandAll(&m)

	rows := gridRows(t, m)
	last := plain(rows[len(rows)-1])
	fields := strings.Fields(last)
	// bound, then one cell per visible scrape; the two newest are the gap.
	if len(fields) < 3 {
		t.Fatalf("grid row = %q, want a bound and its cells", last)
	}
	if fields[len(fields)-1] != "." || fields[len(fields)-2] != "." {
		t.Errorf("row = %q, want the two vanished scrapes to read as gaps", fields)
	}
	if fields[len(fields)-3] != "30" {
		t.Errorf("row = %q, want the last reported value to stay 30", fields)
	}
}

// Shading means "this many observations". A summary's values are latencies, so
// the same scale would read a large p99 as a busy bucket.
func TestSummaryGridIsNotShaded(t *testing.T) {
	withColor(t)
	m := distModel(t, 120, summaryFixture)
	expandAll(&m)

	// The paired positive is TestBusierBucketsShadeBrighter, which proves under
	// this same profile that a histogram grid does carry backgrounds.
	for _, row := range gridRows(t, m) {
		if strings.Contains(row, backgroundSequence) {
			t.Errorf("summary grid row carries a background: %q", row)
		}
	}
}

// A busier bucket must land on a brighter step than a quiet one, or the heatmap
// is decoration rather than information.
func TestBusierBucketsShadeBrighter(t *testing.T) {
	const busy = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 1
lat_seconds_bucket{le="0.5"} 500
lat_seconds_bucket{le="+Inf"} 501
lat_seconds_count 501
`
	withColor(t)
	m := distModel(t, 120, busy)
	m.bucketMode = BucketModePerBucket
	expandAll(&m)

	rows := gridRows(t, m)
	if len(rows) != 4 {
		t.Fatalf("got %d grid rows, want a header and 3 buckets:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	// rows[1] holds 1 observation, rows[2] holds 499 - the block maximum.
	quiet, loud := rows[1], rows[2]
	if !strings.Contains(loud, backgroundSequence) {
		t.Fatalf("the busiest bucket is unshaded: %q", loud)
	}
	if quiet == loud {
		t.Fatal("the quiet and busy buckets rendered identically")
	}
	if shadeFor(1, 499) == shadeFor(499, 499) {
		t.Error("one observation and 499 landed on the same ramp step")
	}
	// An empty band must recede rather than claim the darkest step.
	if shadeFor(0, 499) != nil {
		t.Error("an empty bucket should not be shaded at all")
	}
}

// A native histogram has no buckets to lay out. Saying so beats an empty block,
// which reads as a bug in the view.
func TestNativeHistogramExplainsTheEmptyGrid(t *testing.T) {
	count, sum := uint64(400), 12.5
	schema, zeroThreshold, zeroCount := int32(0), 0.001, uint64(0)
	store := NewStore(10)
	store.UpdateFromFamilies(map[string]*dto.MetricFamily{
		"native_seconds": {
			Name: strPtr("native_seconds"),
			Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{{
				Histogram: &dto.Histogram{
					SampleCount:   &count,
					SampleSum:     &sum,
					Schema:        &schema,
					ZeroThreshold: &zeroThreshold,
					ZeroCount:     &zeroCount,
					PositiveSpan:  []*dto.BucketSpan{{Offset: int32Ptr(0), Length: uint32Ptr(2)}},
					PositiveDelta: []int64{2, 1},
				},
			}},
		},
	})

	m := distModel(t, 120)
	m.store = store
	expandAll(&m)

	content, _ := m.renderDistributions()
	if !strings.Contains(plain(content), nativeHistogramNote) {
		t.Errorf("expanded native histogram = %q, want it labelled unsupported", plain(content))
	}
}

// enter walks collapsed -> expanded -> zoomed and stops; esc walks back down.
// Both must stop at their end rather than cycling round to where the user came
// from, which would make holding the key unpredictable.
func TestEnterAndEscWalkTheLadder(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	sig := m.visibleDistributions()[0].sig

	m.expandStep()
	if !m.expanded[sig] || m.zoomed != "" {
		t.Fatalf("first enter: expanded=%v zoomed=%q, want expanded only", m.expanded[sig], m.zoomed)
	}
	m.expandStep()
	if m.zoomed != sig {
		t.Fatalf("second enter: zoomed=%q, want %q", m.zoomed, sig)
	}
	m.expandStep()
	if m.zoomed != sig || !m.expanded[sig] {
		t.Fatalf("third enter changed state: zoomed=%q expanded=%v", m.zoomed, m.expanded[sig])
	}

	m.collapseStep()
	if m.zoomed != "" || !m.expanded[sig] {
		t.Fatalf("first esc: zoomed=%q expanded=%v, want expanded only", m.zoomed, m.expanded[sig])
	}
	m.collapseStep()
	if m.expanded[sig] {
		t.Fatal("second esc left the family expanded")
	}
	m.collapseStep()
	if m.expanded[sig] || m.zoomed != "" {
		t.Fatal("a third esc re-opened something")
	}
}

// Zoom exists to spend the whole screen on one family, so nothing else may be on
// it - and the family's derived stats have to come along, since the list line
// carrying them is gone.
func TestZoomShowsOneFamilyAndItsStats(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	m.expandStep()
	m.expandStep()

	content, _ := m.renderDistributions()
	text := plain(content)
	if !strings.Contains(text, "a_seconds") {
		t.Fatalf("zoomed content does not name the family:\n%s", text)
	}
	for _, other := range []string{"b_seconds", "c_seconds"} {
		if strings.Contains(text, other) {
			t.Errorf("zoom still shows %s:\n%s", other, text)
		}
	}
	if !strings.Contains(text, "count 6") {
		t.Errorf("zoomed header lost the derived stats:\n%s", text)
	}
	// Nothing is indented under a list any more, so the grid starts at column 0.
	if strings.Contains(text, "\n"+blockIndent) {
		t.Errorf("zoomed grid is still indented:\n%s", text)
	}
}

// A zoom whose family is filtered away must not leave the view stuck on a family
// that is no longer in the list.
func TestZoomIsDroppedWhenItsFamilyGoes(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	m.expandStep()
	m.expandStep()
	if m.zoomed == "" {
		t.Fatal("setup: nothing zoomed")
	}

	m.cfg.FilterMetric = "^b_"
	m.refresh()
	if m.zoomed != "" {
		t.Errorf("zoomed = %q after its family was filtered out, want it cleared", m.zoomed)
	}
	content, _ := m.renderDistributions()
	if !strings.Contains(plain(content), "b_seconds") {
		t.Errorf("view did not fall back to the list:\n%s", plain(content))
	}
}

// Expansion is keyed by signature rather than by row index, so switching views
// and back must not lose it.
func TestExpansionSurvivesAViewRoundTrip(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	m.expandStep()
	sig := m.visibleDistributions()[0].sig

	m.toggleView()
	m.toggleView()
	if !m.expanded[sig] {
		t.Errorf("expansion of %s lost across a v round trip", sig)
	}
}

// An expanded grid must give up columns the same way the collapsed line gives up
// name width, rather than running off the right edge.
func TestExpandedGridsFitTheTerminalWidth(t *testing.T) {
	const wide = `# TYPE lat_seconds histogram
lat_seconds_bucket{handler="/api/v1/products/search/suggestions",le="0.05"} 4
lat_seconds_bucket{handler="/api/v1/products/search/suggestions",le="+Inf"} 9
lat_seconds_count{handler="/api/v1/products/search/suggestions"} 9
`
	for _, width := range []int{40, 60, 80, 120} {
		m := distModel(t, width, wide, wide, wide)
		expandAll(&m)

		// A grid drops time columns until it fits, at every width.
		for _, row := range gridRows(t, m) {
			if got := lipgloss.Width(row); got > width {
				t.Errorf("width %d: grid row is %d columns wide: %q", width, got, plain(row))
			}
		}

		// The collapsed line can only give up name width, and stops at a floor
		// below which two rows are no longer distinguishable. Under about 50
		// columns the six fixed columns simply do not fit; see fitNameColumn.
		if width < 60 {
			continue
		}
		content, _ := m.renderDistributions()
		for _, line := range strings.Split(content, "\n")[1:] {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width %d: line is %d columns wide: %q", width, got, plain(line))
			}
		}
	}
}

// An exporter that publishes only histograms would otherwise look like an
// exporter that publishes nothing.
func TestEmptyMetricsViewPointsAtTheDistributions(t *testing.T) {
	m := distModel(t, 120, latencyFixture)
	m.view = ViewMetrics

	got := plain(m.buildTable())
	if !strings.Contains(got, "press v") {
		t.Errorf("empty metrics table = %q, want it to point at the other view", got)
	}
	if !strings.Contains(got, "1 distribution scraped") {
		t.Errorf("empty metrics table = %q, want the count of distributions", got)
	}
}

// A zoom has to survive a trip to the metrics view and back, since v is how the
// user checks a gauge mid-investigation.
func TestZoomSurvivesAViewRoundTrip(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	m.expandStep()
	m.expandStep()
	sig := m.zoomed
	if sig == "" {
		t.Fatal("setup: nothing zoomed")
	}

	m.toggleView()
	m.toggleView()
	if m.zoomed != sig {
		t.Errorf("zoomed = %q after a round trip, want %q", m.zoomed, sig)
	}
	content, _ := m.renderDistributions()
	if strings.Contains(plain(content), "b_seconds") {
		t.Errorf("the round trip dropped back to the list:\n%s", plain(content))
	}
}

// setViewportHeight shrinks the viewport to h lines and re-lays out the view, so
// a scrolling test does not need a fixture big enough to overflow 20 lines.
func setViewportHeight(m *model, h int) {
	m.viewport.Height = h
	m.refresh()
}

// visibleLines is what the viewport currently shows, which is what the user can
// actually read - the rendered content is longer than the window.
func visibleLines(t *testing.T, m model) []string {
	t.Helper()
	content, _ := m.renderDistributions()
	lines := strings.Split(content, "\n")
	top := m.viewport.YOffset
	bottom := top + m.viewport.Height
	if bottom > len(lines) {
		bottom = len(lines)
	}
	if top > len(lines) {
		top = len(lines)
	}
	return lines[top:bottom]
}

// Expanding the family at the bottom of a full screen must bring its grid on
// screen, not just its name: scrolling follows the block, not the line.
func TestExpandingTheLastFamilyScrollsItsGridIntoView(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	setViewportHeight(&m, 5)

	m.moveCursor(2)
	if m.distCursor != 2 {
		t.Fatalf("setup: cursor = %d, want the last family", m.distCursor)
	}
	m.expandStep()

	shown := strings.Join(visibleLines(t, m), "\n")
	if !strings.Contains(plain(shown), "c_seconds") {
		t.Fatalf("the cursor's own line scrolled off:\n%s", plain(shown))
	}
	// The summary's one quantile row is the last line of its block; if scrolling
	// stopped at the family line it is below the bottom of the window.
	if !strings.Contains(plain(shown), "0.5") {
		t.Errorf("expanded grid is off screen:\n%s", plain(shown))
	}
}

// A block that cannot fit on screen at all is anchored at its top rather than
// left below the fold, and a user who scrolls down inside it keeps their place
// across the next scrape.
func TestTallBlocksAnchorAtTheCursorAndThenStayPut(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	setViewportHeight(&m, 3)

	m.expandStep()
	shown := plain(strings.Join(visibleLines(t, m), "\n"))
	// The window is too short for the whole block, so what it must show is the
	// top of it: the family line followed by the start of its grid.
	if !strings.Contains(shown, "a_seconds") || !strings.Contains(shown, "le") {
		t.Fatalf("block taller than the window was not anchored at the cursor:\n%s", shown)
	}

	m.viewport.SetYOffset(m.viewport.YOffset + 2)
	scrolled := m.viewport.YOffset
	m.refresh()
	if m.viewport.YOffset != scrolled {
		t.Errorf("offset moved from %d to %d on refresh, want the manual scroll kept",
			scrolled, m.viewport.YOffset)
	}
}
