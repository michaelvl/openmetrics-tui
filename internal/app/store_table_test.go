package app

import (
	"testing"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/store"
	"github.com/michaelvl/openmetrics-tui/internal/testutil"
)

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
	st := store.NewStore(10)
	st.UpdateFromFamilies(testutil.ParseFamilies(t, withoutBucket), time.Time{})
	st.UpdateFromFamilies(testutil.ParseFamilies(t, withoutBucket), time.Time{})
	st.UpdateFromFamilies(testutil.ParseFamilies(t, withBucket), time.Time{})

	dist := st.Distributions[`request_duration_seconds{}`]
	if len(dist.Points) != 3 || dist.Points[1].Bound != 0.5 {
		t.Fatalf("new bucket not inserted in sorted position: %v", dist.Points)
	}
	newSeries := dist.Points[1].Series
	if len(newSeries.Values) != 1 {
		t.Fatalf("new bucket should start fresh, got %v", newSeries.Values)
	}

	m := model{cfg: Config{History: 3, DeltaMode: store.DeltaModeOff, LabelMode: LabelModeHideAll}, store: st}
	rows := m.buildTableRows([]*store.MetricSeries{newSeries, dist.Points[0].Series})
	if got := rows[0]; len(got) != 4 || got[1] != "" || got[2] != "" || got[3] != "15" {
		t.Errorf("new bucket row = %q, want the value in the current column", got)
	}
	if got := rows[1]; len(got) != 4 || got[3] != "11" {
		t.Errorf("established bucket row = %q, want its current value in the same column", got)
	}
}
