package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// The aggregation field combines series that differ only in labels nobody asked
// to see. It is one string, in one of these forms:
//
//	pod,instance        sum the series that share every other label
//	avg pod             the same grouping, folded with avg instead of sum
//	count               no grouping labels at all: one row per metric name
//	sum by (pod)        the PromQL spelling of the first form
//	<empty>             off
//
// The operator is optional and defaults to sum, because summing over a pod or
// instance label is the thing people come here to do and a bare label list is
// the fastest thing to type into a header box. The cost is that a label whose
// name happens to be an operator needs "sum sum" to reach, which is a trade
// worth making for a label nobody has.
//
// The "by" is optional too, and the parentheses with it. It adds nothing - a
// by-list is the only kind of grouping here - but it is what anyone who knows
// PromQL types first, and refusing it bought nothing but a puzzle. "without" is
// the one PromQL word that cannot simply be waved through: it names the labels
// to drop rather than the ones to keep, which is the opposite selection and is
// not implemented, so it is refused by name.
//
// Grouping never crosses metric names. PromQL's sum by (pod) drops the metric
// name and would cheerfully add http_requests_total to go_goroutines; in a TUI
// pointed at one exporter that is never what was meant.

type aggOp int

const (
	aggSum aggOp = iota
	aggAvg
	aggMin
	aggMax
	aggCount
)

// aggOps is both the set of names the field accepts and the spelling used to
// decorate an aggregated row's metric name.
var aggOps = map[string]aggOp{
	"sum":   aggSum,
	"avg":   aggAvg,
	"min":   aggMin,
	"max":   aggMax,
	"count": aggCount,
}

func (op aggOp) String() string {
	for name, candidate := range aggOps {
		if candidate == op {
			return name
		}
	}
	return "sum"
}

// aggSpec is a parsed aggregation field. active is false for the empty string,
// which is what makes aggregation a no-op rather than a special case at every
// call site.
type aggSpec struct {
	active bool
	op     aggOp
	by     []string
}

// labelNamePattern is what OpenMetrics allows in a label name. The aggregation
// field is checked against it so that "sum by (env)" - the PromQL spelling, and
// the most likely first attempt - fails with a complaint instead of silently
// grouping by a label called "by (env)" that no series carries.
var labelNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// parseAggregation reads the field. It is deliberately lenient - anything it
// cannot make sense of becomes a grouping label that matches nothing, and
// validateAggregation is what refuses it before it can be applied. That split is
// the same one filter.go draws between matching and validation.
func parseAggregation(value string) aggSpec {
	value = strings.TrimSpace(value)
	if value == "" {
		return aggSpec{}
	}

	spec := aggSpec{active: true, op: aggSum}
	rest := value

	// A leading operator, if there is one. A lone operator name leaves nothing
	// behind, which is what makes "count" one row per metric rather than a group
	// by a label called count.
	if head, tail, ok := cutWord(rest); ok {
		if op, found := aggOps[head]; found {
			spec.op, rest = op, tail
		}
	}
	if head, tail, ok := cutWord(rest); ok && head == "by" {
		rest = tail
	}

	spec.by = splitLabelList(stripParens(rest))
	return spec
}

// cutWord splits off the leading bare word - everything before the first space
// or open parenthesis - and returns it with the remainder trimmed. The
// parenthesis counts as a break so that "by(pod)" reads the same as "by (pod)".
//
// ok is false when there is no word to take, which is the case for an empty
// string and for one that opens with a parenthesis.
func cutWord(s string) (word, rest string, ok bool) {
	i := strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '(' })
	if i < 0 {
		return s, "", s != ""
	}
	return s[:i], strings.TrimSpace(s[i:]), i > 0
}

// stripParens unwraps a parenthesised label list. A list is never nested, so
// matching the outermost pair is all there is to do.
func stripParens(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// splitLabelList breaks a comma-separated label list. Unlike a filter's clauses
// these hold no regexes, so there is nothing here for splitFilterClauses' scanner
// to protect and a plain split is right.
func splitLabelList(list string) []string {
	if list == "" {
		return nil
	}
	names := strings.Split(list, ",")
	for i, name := range names {
		names[i] = strings.TrimSpace(name)
	}
	return names
}

// validateAggregation rejects a field that would otherwise group by a label no
// series carries, which reads as "aggregation did nothing" rather than as a typo.
//
// Messages stay short: they land in the footer next to "⚠ Aggregation: ", and
// what the user typed is already on screen in the box above it.
func validateAggregation(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	// The same three steps parseAggregation takes, so the two cannot disagree
	// about what a field means.
	rest := value
	if head, tail, ok := cutWord(rest); ok {
		if _, found := aggOps[head]; found {
			rest = tail
		}
	}
	if head, tail, ok := cutWord(rest); ok {
		switch head {
		case "by":
			rest = tail
		case "without":
			return errors.New(`"without" not supported`)
		}
	}
	rest = stripParens(rest)

	// Whatever is left should be a comma-separated list and nothing else. A first
	// word with more text after it is one the steps above did not recognise -
	// "avgg pod" - unless a comma is doing the separating, in which case the space
	// is only padding: "pod, instance" is a list, not an operator and a label.
	if head, tail, ok := cutWord(rest); ok && tail != "" &&
		!strings.Contains(head, ",") && !strings.HasPrefix(tail, ",") {
		return errors.New("unknown operator")
	}

	for _, name := range splitLabelList(rest) {
		if name == "" {
			// A stray or trailing comma, refused for the same reason an empty
			// filter clause is.
			return errors.New("empty label name")
		}
		if !labelNamePattern.MatchString(name) {
			return fmt.Errorf("%s: not a label name", truncateToWidth(name, 12))
		}
	}
	return nil
}

// aggregateSeries folds the filtered series according to the aggregation field,
// or returns them untouched when it is empty.
func (m model) aggregateSeries(series []*MetricSeries) []*MetricSeries {
	return parseAggregation(m.cfg.Aggregation).aggregate(series)
}

// aggregate groups the input and folds each group into one synthetic series.
//
// The result is an ordinary *MetricSeries and not a type of its own, which is
// what keeps this change small: ValuesWithDeltas, IsStatic and the whole table
// renderer go on working on an aggregated row exactly as they do on a scraped one.
func (spec aggSpec) aggregate(series []*MetricSeries) []*MetricSeries {
	if !spec.active {
		return series
	}

	groups := make(map[string][]*MetricSeries)
	keys := make([]string, 0, len(series))
	for _, s := range series {
		// The group key reuses the store's own signature function, so it is stable
		// from one scrape to the next and sorts the same way the unaggregated list
		// does - name first, then labels.
		key := GenerateSignature(s.Name, spec.retainLabels(s.Labels))
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], s)
	}
	sort.Strings(keys)

	out := make([]*MetricSeries, 0, len(keys))
	for _, key := range keys {
		out = append(out, spec.fold(groups[key]))
	}
	return out
}

// retainLabels narrows a label set to the grouping labels. A series missing one
// of them keeps it missing rather than gaining an empty value, so that series
// carrying the label and series lacking it land in different groups - and so the
// label cell does not grow a "pod=" for a series that has no pod.
func (spec aggSpec) retainLabels(labels map[string]string) map[string]string {
	if len(spec.by) == 0 {
		return nil
	}
	out := make(map[string]string, len(spec.by))
	for _, key := range spec.by {
		if value, ok := labels[key]; ok {
			out[key] = value
		}
	}
	return out
}

// fold reduces one group to a single series.
func (spec aggSpec) fold(group []*MetricSeries) *MetricSeries {
	first := group[0]

	length := 0
	for _, s := range group {
		if len(s.Values) > length {
			length = len(s.Values)
		}
	}

	return &MetricSeries{
		// Decorating the name is what tells the user a row is derived. Doing it
		// here rather than in the renderer means column widths and styling need no
		// changes, and it is safe because the metric-name filter has already run
		// and never sees the decorated name.
		Name:   fmt.Sprintf("%s(%s)", spec.op, first.Name),
		Kind:   first.Kind,
		Labels: spec.retainLabels(first.Labels),
		Values: spec.foldValues(group, length),
	}
}

// foldValues reduces a group to exactly length values, right-aligned so that the
// last one is this scrape. The length is a parameter rather than the longest
// member, because a distribution folds several groups - one per bucket - that all
// have to land on the same time axis.
func (spec aggSpec) foldValues(group []*MetricSeries, length int) []float64 {
	values := make([]float64, length)
	for i := range values {
		values[i] = spec.foldAt(group, length-1-i)
	}
	return values
}

// foldAt reduces one scrape's worth of a group. offset counts back from the
// newest value rather than forward from the oldest, because a series first seen
// mid-run has a shorter Values slice whose last element is still this scrape -
// the same reason buildTableRows indexes from the tail.
//
// Members that did not report are skipped, so a group is worth reading while one
// of its targets is down. When nobody reported the result is NaN, which the table
// already draws as ".": for count that is "nothing to count here", which is worth
// telling apart from a genuine count of zero.
func (spec aggSpec) foldAt(group []*MetricSeries, offset int) float64 {
	acc := 0.0
	count := 0
	for _, s := range group {
		idx := len(s.Values) - 1 - offset
		if idx < 0 {
			continue
		}
		value := s.Values[idx]
		if math.IsNaN(value) {
			continue
		}
		count++
		switch spec.op {
		case aggSum, aggAvg:
			acc += value
		case aggMin:
			if count == 1 || value < acc {
				acc = value
			}
		case aggMax:
			if count == 1 || value > acc {
				acc = value
			}
		}
	}

	if count == 0 {
		return math.NaN()
	}
	switch spec.op {
	case aggCount:
		return float64(count)
	case aggAvg:
		return acc / float64(count)
	}
	return acc
}

// Distributions aggregate too, but only with sum, and only histograms.
//
// A histogram's buckets are observation counts over shared bounds, so adding two
// families bucket by bucket yields exactly the histogram one process would have
// reported had it done all the work - which is what makes the folded family's
// quantiles, count and rate worth reading. No other operator survives that test:
// the average or the maximum of two bucket counts is not the histogram of
// anything, and a p99 estimated from it would describe a distribution that never
// existed. A non-sum operator therefore leaves this view's list alone rather than
// dressing up nonsense as latencies, and the view's header says so.
//
// Summaries are left alone for the same reason: their points are quantiles the
// exporter already computed, and no arithmetic on two p99s gives the p99 of the
// combined stream.

// aggregateDistributions folds the visible families according to the aggregation
// field. Entries it cannot fold come back untouched, so the list stays complete
// whatever the field says.
func (m model) aggregateDistributions(entries []distEntry) []distEntry {
	return parseAggregation(m.cfg.Aggregation).aggregateDistributions(entries)
}

func (spec aggSpec) aggregateDistributions(entries []distEntry) []distEntry {
	if !spec.active || spec.op != aggSum {
		return entries
	}

	groups := make(map[string][]*DistributionSeries)
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		// A summary keys on its own store signature, so it lands in a group of one
		// and comes back out as itself. A histogram keys the way the scalar fold
		// does: by name and the grouping labels, never across names.
		key := entry.sig
		if entry.dist.Kind == KindHistogram {
			key = GenerateSignature(entry.dist.Name, spec.retainLabels(entry.dist.Labels))
		}
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], entry.dist)
	}
	sort.Strings(keys)

	out := make([]distEntry, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		// The key is what the accordion records expansion and zoom under, so a
		// folded family has to carry the group key rather than any member's
		// signature: two members must not each claim their own expanded state for
		// the one row they now share.
		entry := distEntry{sig: key, dist: group[0]}
		if group[0].Kind == KindHistogram {
			entry.dist = spec.foldDistributions(group)
		}
		out = append(out, entry)
	}
	return out
}

// foldDistributions sums a group of histogram families into one synthetic family.
// The result is an ordinary *DistributionSeries, so the grid, the shading, the
// quantile estimator and hide-static all go on working on it unchanged.
func (spec aggSpec) foldDistributions(group []*DistributionSeries) *DistributionSeries {
	first := group[0]
	labels := spec.retainLabels(first.Labels)
	out := &DistributionSeries{
		// Decorated for the same reason an aggregated table row is: it is the only
		// thing on the line that says these numbers are derived. The metric-name
		// filter has already run and never sees the decorated name.
		Name:   fmt.Sprintf("%s(%s)", spec.op, first.Name),
		Kind:   first.Kind,
		Labels: labels,
	}

	// One length for the whole folded family, so a grid column means the same
	// scrape in every bucket row even when members have retained different
	// histories - the same alignment valueAt gives a scraped family.
	length := 0
	for _, dist := range group {
		if n := distScrapeCount(dist); n > length {
			length = n
		}
	}

	for _, bound := range unionBounds(group) {
		members := make([]*MetricSeries, 0, len(group))
		for _, dist := range group {
			if series := dist.findPoint(bound); series != nil {
				members = append(members, series)
			}
		}
		out.insertPoint(bound, &MetricSeries{
			Name:   out.Name + "_bucket",
			Kind:   KindCounter,
			Labels: withLabel(labels, "le", formatBound(bound)),
			Values: spec.foldValues(members, length),
		})
	}

	out.Sum = spec.foldPart(group, out.Name+"_sum", labels, length,
		func(dist *DistributionSeries) *MetricSeries { return dist.Sum })
	out.Count = spec.foldPart(group, out.Name+"_count", labels, length,
		func(dist *DistributionSeries) *MetricSeries { return dist.Count })
	return out
}

// unionBounds is every bound any member reports, ascending with +Inf last, which
// is the order Points must keep.
//
// Bounds are unioned rather than intersected, and a bound only some members carry
// is summed over the members that carry it. That reads low at such a bound, since
// the members without it have observations below it that go uncounted there - the
// same trade PromQL's sum by (le) makes, and the price of staying readable while
// one target's bucket layout differs or a new bucket appears mid-run. A native
// histogram has no bounds at all and so contributes nothing here; only its count
// reaches the folded family.
func unionBounds(group []*DistributionSeries) []float64 {
	seen := make(map[float64]bool)
	bounds := make([]float64, 0, len(group[0].Points))
	for _, dist := range group {
		for _, point := range dist.Points {
			if seen[point.Bound] {
				continue
			}
			seen[point.Bound] = true
			bounds = append(bounds, point.Bound)
		}
	}
	sort.Float64s(bounds)
	return bounds
}

// foldPart sums one of a family's derived series - its sum or its count - across
// the group, returning nil when no member reports one. A nil Count is how the
// view tells "the exporter publishes none" from "it publishes zero", so the fold
// has to preserve that rather than produce an all-NaN series.
func (spec aggSpec) foldPart(group []*DistributionSeries, name string, labels map[string]string, length int, pick func(*DistributionSeries) *MetricSeries) *MetricSeries {
	members := make([]*MetricSeries, 0, len(group))
	for _, dist := range group {
		if series := pick(dist); series != nil {
			members = append(members, series)
		}
	}
	if len(members) == 0 {
		return nil
	}
	return &MetricSeries{
		Name:   name,
		Kind:   KindCounter,
		Labels: copyLabels(labels),
		Values: spec.foldValues(members, length),
	}
}

// distAggNote explains what the aggregation field did to this view in the two
// cases the rows alone do not say: an operator this view will not honour, and
// summaries it had to leave alone while folding the histograms around them.
func (m model) distAggNote(entries []distEntry) string {
	spec := parseAggregation(m.cfg.Aggregation)
	if !spec.active {
		return ""
	}
	if spec.op != aggSum {
		return fmt.Sprintf("%s ignored: sum only", spec.op)
	}
	for _, entry := range entries {
		if entry.dist.Kind == KindSummary {
			return "summaries not summed"
		}
	}
	return ""
}
