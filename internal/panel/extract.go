package panel

import (
	"math"
	"sort"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

// maxPlanPoints keeps a planned range well under Prometheus' 11,000 point limit.
const maxPlanPoints = 10000

// staleSteps is how many steps the last sample may trail the range end before
// a series counts as gone and its now column is empty. A vanished series gets
// a staleness marker, so the backend stops returning points for it.
const staleSteps = 2

// Range is the query_range window for a panel. Points are at End - k*Step.
type Range struct {
	Start, End time.Time
	Step       time.Duration
}

// PlanRange picks the range [end - max(windows, spark), end] and a step. The
// step is chosen so the window offsets land on step boundaries where
// possible: it divides the smallest nonzero offset (the other offsets are
// normally multiples of it) and is the divisor nearest to the preferred step
// from promapi.Step(width, minStep). If no offset is usable, or the divisor
// would give too many points, the preferred step is used and offsets snap to
// the nearest step. Start is moved back to a whole number of steps before
// end, so every point sits on the grid anchored at end.
func PlanRange(windows []dashboard.Window, spark time.Duration, end time.Time, width int, minStep time.Duration) Range {
	span := spark
	var base time.Duration
	for _, w := range windows {
		span = max(span, w.Offset)
		if w.Offset > 0 && (base == 0 || w.Offset < base) {
			base = w.Offset
		}
	}
	if span <= 0 {
		span = dashboard.DefaultSpark
	}
	target := promapi.Step(end.Add(-span), end, width, minStep)
	step := target
	if base > 0 {
		if d := nearestDivisor(base, target); d > 0 && span/d <= maxPlanPoints {
			step = d
		}
	}
	n := (span + step - 1) / step
	return Range{Start: end.Add(-n * step), End: end, Step: step}
}

// nearestDivisor returns the whole-millisecond divisor of base closest to
// target, or 0 if there is none.
func nearestDivisor(base, target time.Duration) time.Duration {
	var best time.Duration
	kmax := int64(base/target)*2 + 2
	for k := int64(1); k <= kmax; k++ {
		if base%time.Duration(k) != 0 {
			continue
		}
		d := base / time.Duration(k)
		if d%time.Millisecond != 0 {
			continue
		}
		if best == 0 || absDur(d-target) < absDur(best-target) {
			best = d
		}
	}
	return best
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// Cell is one window column of a row. V is NaN when there is no value.
type Cell struct {
	V float64
	// Before is set when the window reaches back before the series' first
	// sample (cold start, or a series that appeared recently). V is NaN.
	Before bool
}

// Row is one series of a panel.
type Row struct {
	Title  string // rendered label template
	Labels map[string]string
	// Cells is parallel to the windows given to Extract; Cells[i] with a
	// zero offset is the now column.
	Cells []Cell
	// Spark has one slot per step over the spark window, oldest first, NaN
	// where there is no sample. Nil when spark is zero.
	Spark []float64
	// Earliest and Latest are the times of the first and last non-NaN
	// samples in the response; both are zero for a series with none.
	Earliest, Latest time.Time
}

// Now returns the value of the first column with a zero offset, or NaN.
func (r Row) Now(windows []dashboard.Window) float64 {
	for i, w := range windows {
		if w.Offset == 0 && i < len(r.Cells) {
			return r.Cells[i].V
		}
	}
	return math.NaN()
}

// Extract turns a range query response into rows, one per series, in the
// response order.
//
// Column values: the now column (offset 0) is the last non-NaN sample, unless
// that is more than two steps older than the range end, in which case the
// series is considered gone and the value is NaN. Other columns take the
// sample nearest to end-offset (within half a step), or, when agg is "avg",
// "max" or "min", the aggregate of the non-NaN samples in [end-offset, end].
// Gaps and NaN samples give NaN cells. A column reaching back before the
// series' earliest sample has Before set and no value, in either mode.
func Extract(p dashboard.Panel, series []promapi.Series, windows []dashboard.Window, spark time.Duration, r Range) []Row {
	rows := make([]Row, 0, len(series))
	for _, s := range series {
		rows = append(rows, extractRow(p, s, windows, spark, r))
	}
	return rows
}

func extractRow(p dashboard.Panel, s promapi.Series, windows []dashboard.Window, spark time.Duration, r Range) Row {
	pts := make([]promapi.Sample, 0, len(s.Points))
	for _, pt := range s.Points {
		if !math.IsNaN(pt.V) {
			pts = append(pts, pt)
		}
	}
	sort.SliceStable(pts, func(a, b int) bool { return pts[a].T.Before(pts[b].T) })

	row := Row{Title: Label(p.Label, s.Labels), Labels: s.Labels, Cells: make([]Cell, len(windows))}
	if len(pts) > 0 {
		row.Earliest, row.Latest = pts[0].T, pts[len(pts)-1].T
	}
	half := r.Step / 2
	for i, w := range windows {
		c := Cell{V: math.NaN()}
		switch {
		case len(pts) == 0:
		case w.Offset == 0:
			if r.End.Sub(row.Latest) <= staleSteps*r.Step {
				c.V = pts[len(pts)-1].V
			}
		case row.Earliest.After(r.End.Add(-w.Offset).Add(half)):
			c.Before = true
		case p.Agg != "":
			c.V = aggregate(p.Agg, pts, r.End.Add(-w.Offset).Add(-half), r.End)
		default:
			c.V = nearest(pts, r.End.Add(-w.Offset), half)
		}
		row.Cells[i] = c
	}
	if spark > 0 {
		n := int(spark/r.Step) + 1
		row.Spark = make([]float64, n)
		for i := range row.Spark {
			row.Spark[i] = math.NaN()
		}
		for _, pt := range pts {
			back := int(math.Round(float64(r.End.Sub(pt.T)) / float64(r.Step)))
			if idx := n - 1 - back; idx >= 0 && idx < n {
				row.Spark[idx] = pt.V
			}
		}
	}
	return row
}

// nearest returns the value of the sample closest to t within tol, or NaN.
func nearest(pts []promapi.Sample, t time.Time, tol time.Duration) float64 {
	i := sort.Search(len(pts), func(i int) bool { return !pts[i].T.Before(t) })
	best, bestD := math.NaN(), tol+1
	for _, j := range []int{i - 1, i} {
		if j < 0 || j >= len(pts) {
			continue
		}
		if d := absDur(pts[j].T.Sub(t)); d <= tol && d < bestD {
			best, bestD = pts[j].V, d
		}
	}
	return best
}

// aggregate applies agg to the samples with from <= T <= to.
func aggregate(agg string, pts []promapi.Sample, from, to time.Time) float64 {
	var sum float64
	n := 0
	out := math.NaN()
	for _, pt := range pts {
		if pt.T.Before(from) || pt.T.After(to) {
			continue
		}
		n++
		sum += pt.V
		switch {
		case n == 1:
			out = pt.V
		case agg == "max":
			out = math.Max(out, pt.V)
		case agg == "min":
			out = math.Min(out, pt.V)
		}
	}
	if n == 0 {
		return math.NaN()
	}
	if agg == "avg" {
		return sum / float64(n)
	}
	return out
}

// Earliest returns the earliest sample time over all rows, or the zero time.
// The cold-start note uses it to say how much history exists.
func Earliest(rows []Row) time.Time {
	var e time.Time
	for _, r := range rows {
		if !r.Earliest.IsZero() && (e.IsZero() || r.Earliest.Before(e)) {
			e = r.Earliest
		}
	}
	return e
}
