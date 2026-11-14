package panel

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
)

// Label renders a row title from a template with {{labelname}}
// substitutions. An empty template, or one naming a label the series does not
// have, falls back to {k=v,...} with the keys sorted.
func Label(tmpl string, labels map[string]string) string {
	if tmpl == "" {
		return fallbackLabel(labels)
	}
	var b strings.Builder
	rest := tmpl
	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end := strings.Index(rest[open:], "}}")
		if end < 0 {
			return fallbackLabel(labels)
		}
		val, ok := labels[strings.TrimSpace(rest[open+2:open+end])]
		if !ok {
			return fallbackLabel(labels)
		}
		b.WriteString(rest[:open])
		b.WriteString(val)
		rest = rest[open+end+2:]
	}
}

func fallbackLabel(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Color returns the colour name of the first threshold that matches the raw
// value v, or "" when none does (or v is NaN, which matches nothing, not even
// a catch-all).
func Color(ts []dashboard.Threshold, v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	for _, t := range ts {
		var match bool
		switch t.Op {
		case "gt":
			match = v > t.Value
		case "gte":
			match = v >= t.Value
		case "lt":
			match = v < t.Value
		case "lte":
			match = v <= t.Value
		case "":
			match = true
		}
		if match {
			return t.Color
		}
	}
	return ""
}

// FormatValue renders a cell value for the panel unit. "percent" multiplies
// by 100 and appends %; "s", "/min" and "" give the plain number (the unit is
// shown in the panel title). NaN is "-".
func FormatValue(unit string, v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	if unit == "percent" {
		return formatFloat(v*100) + "%"
	}
	return formatFloat(v)
}

// formatFloat is a copy of the helper of the same name in internal/app (see
// the plan, Decisions: shared helpers). It renders a value exactly enough that
// small numbers stay distinguishable from zero. Integers are exact; fractions
// carry roughly three significant digits without ever dropping an integer
// digit, so 0.0034 stays 0.0034 and 21203 stays 21203.
func formatFloat(val float64) string {
	if math.IsNaN(val) {
		return "NaN"
	}
	abs := math.Abs(val)
	if math.IsInf(val, 0) || abs >= 1e15 {
		return strconv.FormatFloat(val, 'g', 3, 64)
	}
	if val == math.Trunc(val) {
		return strconv.FormatFloat(val, 'f', -1, 64)
	}
	var s string
	switch {
	case abs >= 100:
		s = strconv.FormatFloat(val, 'f', 0, 64)
	case abs >= 10:
		s = strconv.FormatFloat(val, 'f', 1, 64)
	case abs >= 1:
		s = strconv.FormatFloat(val, 'f', 2, 64)
	default:
		s = strconv.FormatFloat(val, 'g', 3, 64)
	}
	return trimTrailingZeros(s)
}

// trimTrailingZeros drops the padding "%f" adds, but only from a fractional
// part. Copy of the helper in internal/app.
func trimTrailingZeros(s string) string {
	if !strings.Contains(s, ".") || strings.ContainsAny(s, "eE") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// SortMode is the interactive row order; it cycles with the s key.
type SortMode int

const (
	SortYAML    SortMode = iota // the panel's sort: field (none keeps response order)
	SortNowDesc                 // now column, highest first
	SortNowAsc                  // now column, lowest first
	SortLabel                   // row title, A to Z
)

// Next returns the mode after m in the cycle.
func (m SortMode) Next() SortMode { return (m + 1) % 4 }

// Arrange orders rows and applies the panel limit, returning a new slice. In
// SortYAML mode the order is p.Sort ("asc" or "desc" on the now column; "" keeps
// the response order). The limit is applied after sorting, so limit with
// sort: desc is top N. Rows without a now value sort last; ties keep their
// input order.
func Arrange(rows []Row, p dashboard.Panel, windows []dashboard.Window, mode SortMode) []Row {
	out := append([]Row(nil), rows...)
	desc, asc, byLabel := false, false, false
	switch mode {
	case SortYAML:
		desc, asc = p.Sort == "desc", p.Sort == "asc"
	case SortNowDesc:
		desc = true
	case SortNowAsc:
		asc = true
	case SortLabel:
		byLabel = true
	}
	switch {
	case byLabel:
		sort.SliceStable(out, func(a, b int) bool { return out[a].Title < out[b].Title })
	case desc || asc:
		sort.SliceStable(out, func(a, b int) bool {
			va, vb := out[a].Now(windows), out[b].Now(windows)
			na, nb := math.IsNaN(va), math.IsNaN(vb)
			if na || nb {
				return !na && nb
			}
			if desc {
				return va > vb
			}
			return va < vb
		})
	}
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out
}
