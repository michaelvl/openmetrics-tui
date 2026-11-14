package panel

import (
	"math"
	"strings"
)

var blocks = []rune("▁▂▃▄▅▆▇█")

// Missing marks a sparkline cell without data. It sits on the baseline, as
// the lowest block does, so it reads as empty rather than as a value.
const Missing = '.'

// Sparkline renders values as exactly n block characters. Values are
// resampled to n cells (cells covering several values average them; with
// fewer values than cells each value is stretched), then scaled between the
// minimum and maximum of the data. NaN and infinite values and cells without
// data are Missing. A flat series (all values equal) is the lowest block, and
// an empty or all-NaN series is n Missing cells, so columns stay aligned.
func Sparkline(values []float64, n int) string {
	if n <= 0 {
		return ""
	}
	cells := resample(values, n)
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range cells {
		if !math.IsNaN(v) {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	var b strings.Builder
	for _, v := range cells {
		switch {
		case math.IsNaN(v):
			b.WriteRune(Missing)
		case hi == lo:
			b.WriteRune(blocks[0])
		default:
			i := int((v - lo) / (hi - lo) * float64(len(blocks)))
			b.WriteRune(blocks[min(i, len(blocks)-1)])
		}
	}
	return b.String()
}

// resample maps values onto n cells; the result has NaN where no finite value
// falls in a cell.
func resample(values []float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if len(values) == 0 {
		return out
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if len(values) <= n {
		for i := range out {
			if v := values[i*len(values)/n]; finite(v) {
				out[i] = v
			}
		}
		return out
	}
	for i := range out {
		from, to := i*len(values)/n, (i+1)*len(values)/n
		var sum float64
		cnt := 0
		for _, v := range values[from:to] {
			if finite(v) {
				sum += v
				cnt++
			}
		}
		if cnt > 0 {
			out[i] = sum / float64(cnt)
		}
	}
	return out
}
