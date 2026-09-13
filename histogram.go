package main

import (
	"math"
	"time"
)

// BucketMode selects how a histogram's bucket counts are presented across the
// bounds of a single scrape. It is one of the display's two independent axes:
// this one runs down the column, from the lowest bound to +Inf, while the delta
// mode runs along the row, across time. Neither one knows about the other, so
// any pairing of the two means what both names say it means.
//
// Buckets arrive cumulative ("le=0.1" includes everything in "le=0.05"), which
// is the exporter's own layout rather than anything this tool computes. The
// second mode undoes it.
type BucketMode int

const (
	// BucketModeCumulative shows the counts exactly as scraped.
	BucketModeCumulative BucketMode = iota
	// BucketModePerBucket subtracts each bucket from the one below it, so a row
	// counts only the observations that fell in that band. The counts are still
	// lifetime totals; turning those into a rate is the delta mode's job.
	BucketModePerBucket
)

func (b BucketMode) String() string {
	switch b {
	case BucketModeCumulative:
		return "Cumulative"
	case BucketModePerBucket:
		return "Per bucket"
	}
	return "unknown"
}

// next toggles cumulative <-> per-bucket.
func (b BucketMode) next() BucketMode {
	if b == BucketModeCumulative {
		return BucketModePerBucket
	}
	return BucketModeCumulative
}

// displayQuantiles are the quantiles shown on a collapsed distribution line, and
// displayQuantileNames their column headings. The two are parallel and are the
// single source of both, so adding a quantile cannot silently mislabel a column.
var (
	displayQuantiles     = []float64{0.5, 0.9, 0.99}
	displayQuantileNames = []string{"p50", "p90", "p99"}
)

// IsStatic reports whether nothing in the family moves, so the hide-static
// toggle drops a distribution only when every bucket, the sum and the count are
// all unchanging.
func (d *DistributionSeries) IsStatic() bool {
	static, seen := true, false
	d.eachSeries("", func(_ string, series *MetricSeries) {
		seen = true
		if !series.IsStatic() {
			static = false
		}
	})
	return seen && static
}

// distScrapeCount is the number of retained scrapes the family spans, which is
// the longest history any of its series has. Series are right-aligned: a bucket
// the exporter only started reporting mid-run has a shorter history, and reads
// as missing for the scrapes before it existed.
func distScrapeCount(dist *DistributionSeries) int {
	total := 0
	dist.eachSeries("", func(_ string, series *MetricSeries) {
		if len(series.Values) > total {
			total = len(series.Values)
		}
	})
	return total
}

// valueAt reads one series at scrape index idx, where index 0 is the oldest of
// total retained scrapes. A series shorter than total is padded on the left with
// NaN rather than shifted, so a column always means the same scrape across the
// whole family.
func valueAt(series *MetricSeries, idx, total int) float64 {
	if series == nil || idx < 0 || idx >= total {
		return math.NaN()
	}
	offset := total - 1 - idx
	pos := len(series.Values) - 1 - offset
	if pos < 0 || pos >= len(series.Values) {
		return math.NaN()
	}
	return series.Values[pos]
}

// bucketDisplayValues returns one value row per bucket or quantile, in Points
// order, every row padded to the family's full scrape count.
//
// The display's two axes compose here, in a fixed order and without consulting
// each other: the bucket mode collapses each scrape down its bounds, then the
// delta mode walks each row across time. Summary quantiles are latencies rather
// than cumulative counts, so decumulating them would be meaningless and the
// bucket mode passes them through untouched; the delta mode still applies.
func bucketDisplayValues(dist *DistributionSeries, mode BucketMode, deltaMode string) [][]float64 {
	total := distScrapeCount(dist)
	rows := make([][]float64, len(dist.Points))
	for i, point := range dist.Points {
		row := make([]float64, total)
		for idx := range row {
			row[idx] = valueAt(point.Series, idx, total)
		}
		rows[i] = row
	}

	if dist.Kind == KindHistogram && mode == BucketModePerBucket {
		for idx := 0; idx < total; idx++ {
			decumulateScrape(rows, idx)
		}
	}

	if deltaMode != DeltaModeOff {
		// Borrow the simple-metric time transform rather than reimplementing its
		// modes and their NaN handling. The kind travels with the row so that a
		// bucket's counter reset is blanked and a summary's falling quantile is
		// not.
		for i, point := range dist.Points {
			kind := KindCounter
			if point.Series != nil {
				kind = point.Series.Kind
			}
			rows[i] = (&MetricSeries{Kind: kind, Values: rows[i]}).ValuesWithDeltas(deltaMode)
		}
	}
	return rows
}

// decumulateScrape rewrites one column of cumulative bucket counts into
// per-bucket counts. Buckets missing at this scrape are left alone and do not
// become the baseline, so the band below simply spans a wider range.
func decumulateScrape(rows [][]float64, idx int) {
	base, haveBase := 0.0, false
	for i := range rows {
		cumulative := rows[i][idx]
		if math.IsNaN(cumulative) {
			continue
		}
		band := cumulative
		if haveBase {
			band = cumulative - base
		}
		base, haveBase = cumulative, true
		if band < 0 {
			// A broken exporter reported a bucket smaller than the one below it.
			// Clamp rather than render a negative observation count.
			band = 0
		}
		rows[i][idx] = band
	}
}

// quantileCell is one estimated or reported quantile.
type quantileCell struct {
	// Value is the quantile's value, or the last finite bucket bound when Beyond.
	Value float64
	// Beyond is set when the rank fell in the +Inf bucket, where there is no
	// upper bound to interpolate towards. Value is then only a lower bound.
	Beyond bool
	// OK is false when no estimate could be made at all.
	OK bool
}

// estimateQuantile reports the q-th quantile of the family at scrape index
// scrapeIdx.
//
// Summaries carry their quantiles directly, so the requested one is read off
// when the exporter reports it. Histograms only carry bucket counts, so the
// value is estimated the way histogram_quantile does: find the bucket holding
// rank q*total and interpolate linearly between its bound and the one below.
// Precision is therefore bounded by the bucket layout - a p99 of 0.87 derived
// from bounds [0.5, 1] really only means "somewhere in 0.5 to 1.0".
func estimateQuantile(dist *DistributionSeries, scrapeIdx int, q float64) quantileCell {
	total := distScrapeCount(dist)
	if len(dist.Points) == 0 || scrapeIdx < 0 || scrapeIdx >= total {
		return quantileCell{}
	}

	if dist.Kind == KindSummary {
		for _, point := range dist.Points {
			if point.Bound != q {
				continue
			}
			val := valueAt(point.Series, scrapeIdx, total)
			if math.IsNaN(val) {
				return quantileCell{}
			}
			return quantileCell{Value: val, OK: true}
		}
		// The exporter does not publish this quantile; it cannot be derived.
		return quantileCell{}
	}

	observations, ok := bucketTotal(dist, scrapeIdx)
	if !ok || observations <= 0 {
		return quantileCell{}
	}
	rank := q * observations

	prevBound, prevCount := 0.0, 0.0
	running := 0.0
	lastFinite := 0.0
	for _, point := range dist.Points {
		count := valueAt(point.Series, scrapeIdx, total)
		if math.IsNaN(count) {
			continue
		}
		if count < running {
			// Cumulative counts must not decrease. Clamp a broken exporter to the
			// running maximum instead of producing a wild interpolation.
			count = running
		}
		running = count

		if count >= rank {
			if math.IsInf(point.Bound, +1) {
				return quantileCell{Value: lastFinite, Beyond: true, OK: true}
			}
			if count == prevCount {
				// An empty band cannot be interpolated across; the bound itself is
				// the best answer.
				return quantileCell{Value: point.Bound, OK: true}
			}
			fraction := (rank - prevCount) / (count - prevCount)
			return quantileCell{Value: prevBound + fraction*(point.Bound-prevBound), OK: true}
		}

		prevCount = count
		if !math.IsInf(point.Bound, +1) {
			prevBound = point.Bound
			lastFinite = point.Bound
		}
	}

	// No bucket reached the rank, which means the count series disagrees with the
	// buckets. Report the last bound we saw as a lower bound.
	return quantileCell{Value: lastFinite, Beyond: true, OK: true}
}

// bucketTotal is the observation count the buckets themselves imply at
// scrapeIdx: the largest cumulative count, normally the +Inf bucket. Quantile
// ranks are compared against bucket counts, so they must be derived from the
// same source rather than from a _count series that may disagree.
func bucketTotal(dist *DistributionSeries, scrapeIdx int) (float64, bool) {
	total := distScrapeCount(dist)
	best, ok := 0.0, false
	for _, point := range dist.Points {
		val := valueAt(point.Series, scrapeIdx, total)
		if math.IsNaN(val) {
			continue
		}
		if !ok || val > best {
			best, ok = val, true
		}
	}
	return best, ok
}

// distCount is the observation count to display: the exporter's _count series
// when it reports one, falling back to the buckets. Native histograms carry no
// buckets at all, so the _count path is the only one that works for them.
func distCount(dist *DistributionSeries, scrapeIdx int) (float64, bool) {
	if dist.Count != nil {
		if val := valueAt(dist.Count, scrapeIdx, distScrapeCount(dist)); !math.IsNaN(val) {
			return val, true
		}
	}
	if dist.Kind == KindSummary {
		// A summary's points are latencies, not counts; there is nothing to fall
		// back to.
		return 0, false
	}
	return bucketTotal(dist, scrapeIdx)
}

// distStats is the collapsed one-line view of a distribution.
type distStats struct {
	Count   float64
	CountOK bool
	// Rate is observations per second since the previous scrape that reported a
	// count.
	Rate   float64
	RateOK bool
	// Cells holds one entry per displayQuantiles entry, in that order.
	Cells []quantileCell
}

// distSummary derives the collapsed line for a distribution at scrape index
// scrapeIdx. interval is the polling interval, needed to turn a count delta into
// a rate.
func distSummary(dist *DistributionSeries, scrapeIdx int, interval time.Duration) distStats {
	stats := distStats{Cells: make([]quantileCell, 0, len(displayQuantiles))}
	for _, q := range displayQuantiles {
		stats.Cells = append(stats.Cells, estimateQuantile(dist, scrapeIdx, q))
	}

	count, ok := distCount(dist, scrapeIdx)
	if !ok {
		return stats
	}
	stats.Count, stats.CountOK = count, true

	// Walk back to the most recent scrape that reported a count; holes in the
	// history widen the window rather than invalidating the rate.
	for idx := scrapeIdx - 1; idx >= 0; idx-- {
		prev, prevOK := distCount(dist, idx)
		if !prevOK {
			continue
		}
		elapsed := interval.Seconds() * float64(scrapeIdx-idx)
		if elapsed > 0 && count >= prev {
			stats.Rate, stats.RateOK = (count-prev)/elapsed, true
		}
		// Either way this was the comparison scrape; a reset simply has no rate.
		break
	}
	return stats
}
