package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// MetricKind identifies the OpenMetrics/Prometheus type a series originates from.
// Gauge, counter and untyped metrics are scalars and live in Store.Metrics;
// histograms and summaries are distributions and live in Store.Distributions.
type MetricKind int

const (
	KindGauge MetricKind = iota
	KindCounter
	KindUntyped
	KindHistogram
	KindSummary
)

func (k MetricKind) String() string {
	switch k {
	case KindGauge:
		return "gauge"
	case KindCounter:
		return "counter"
	case KindUntyped:
		return "untyped"
	case KindHistogram:
		return "histogram"
	case KindSummary:
		return "summary"
	}
	return "unknown"
}

type MetricSeries struct {
	Name   string
	Kind   MetricKind
	Labels map[string]string
	Values []float64
}

// ValuesWithDeltas returns the values, optionally converting them to deltas based on the mode.
// Modes:
// - "off": Returns raw absolute values
// - "next": Historical values are deltas to next value (val[i+1] - val[i]), current is absolute
// - "view": All values are deltas; historical same as "next", current is the growth across the whole window (last - first)
//
// A counter that goes backwards has been reset, and the drop is an artefact of
// the restart rather than something that was measured, so it is reported as
// missing rather than as a large negative number. A gauge is free to fall -
// that is what a gauge is for - so the guard is keyed on the series kind.
//
// epochs holds the scrape epoch of each value, right-aligned to Values the same
// way every series is (see Store.Epochs). Two samples from different epochs were
// taken either side of a failed scrape, most likely from two different
// processes, so no delta is taken between them whatever the kind. nil means the
// whole history is one epoch.
func (s *MetricSeries) ValuesWithDeltas(mode string, epochs []int) []float64 {
	if mode == "off" {
		return s.Values
	}

	if len(s.Values) == 0 {
		return nil
	}

	res := make([]float64, len(s.Values))
	lastIdx := len(s.Values) - 1

	// Handle historical values (all modes with deltas)
	// Previous elements are deltas to the next element
	for i := 0; i < lastIdx; i++ {
		curr := s.Values[i]
		next := s.Values[i+1]
		if math.IsNaN(curr) || math.IsNaN(next) || s.isReset(curr, next) || !s.sameEpoch(epochs, i, i+1) {
			res[i] = math.NaN()
		} else {
			res[i] = next - curr
		}
	}

	// Handle the current/last value based on mode
	if mode == "view" {
		res[lastIdx] = s.viewSpan(epochs)
	} else {
		// In "next" mode, last element is absolute
		res[lastIdx] = s.Values[lastIdx]
	}

	return res
}

// viewSpan is the growth across everything on screen: the newest sample less the
// oldest one. The newest sample counts - the delta it earned is already on screen
// in the column beside the current one, so leaving it out would make the current
// column lag a scrape behind the row it sums up.
//
// Only the newest epoch is spanned. Growth across a reconnect would add up two
// processes' counts, while growth since the reconnect is exactly what is worth
// knowing right after one.
//
// NaN when the span holds fewer than two samples, or when a counter reset
// inside it makes the span an artefact of a restart rather than a measurement.
func (s *MetricSeries) viewSpan(epochs []int) float64 {
	firstIdx, lastPresentIdx := -1, -1
	for i, v := range s.Values {
		if math.IsNaN(v) {
			continue
		}
		if firstIdx == -1 || !s.sameEpoch(epochs, firstIdx, i) {
			firstIdx = i
		} else if s.isReset(s.Values[lastPresentIdx], v) {
			return math.NaN()
		}
		lastPresentIdx = i
	}
	if firstIdx == -1 || firstIdx == lastPresentIdx {
		return math.NaN()
	}
	return s.Values[lastPresentIdx] - s.Values[firstIdx]
}

// sameEpoch reports whether values i and j were scraped in the same epoch.
// Positions epochs does not reach are treated as matching, so a caller without
// scrape bookkeeping gets the plain behaviour.
func (s *MetricSeries) sameEpoch(epochs []int, i, j int) bool {
	a, okA := epochAt(epochs, len(s.Values), i)
	b, okB := epochAt(epochs, len(s.Values), j)
	return !okA || !okB || a == b
}

// epochAt maps index i of an n-long right-aligned history onto epochs.
func epochAt(epochs []int, n, i int) (int, bool) {
	pos := len(epochs) - n + i
	if pos < 0 || pos >= len(epochs) {
		return 0, false
	}
	return epochs[pos], true
}

// isReset reports whether the series fell between two samples in a way that can
// only mean the exporting process restarted. Only counters qualify; a falling
// gauge is an ordinary measurement.
func (s *MetricSeries) isReset(from, to float64) bool {
	return s.Kind == KindCounter && to < from
}

// IsStatic reports whether all retained non-NaN values are equal.
// Returns false if fewer than 2 non-NaN samples are available (not enough
// data to judge).
func (s *MetricSeries) IsStatic() bool {
	first := math.NaN()
	count := 0
	for _, v := range s.Values {
		if math.IsNaN(v) {
			continue
		}
		if count == 0 {
			first = v
		} else if v != first {
			return false
		}
		count++
	}
	return count >= 2
}

// DistributionSeries is a single histogram or summary family with one label set.
// Histogram buckets and summary quantiles have the same shape on the wire - a
// value per bound, plus a sum and a count - so both are stored here, told apart
// by Kind.
//
// Every series inside is an ordinary MetricSeries carrying the name and labels
// the text format uses (name_bucket with an "le" label, name_sum, name_count),
// so the same history handling and rendering code that serves simple metrics
// applies unchanged.
type DistributionSeries struct {
	Name   string              // family name, without the _bucket/_sum/_count suffix
	Kind   MetricKind          // KindHistogram or KindSummary
	Labels map[string]string   // labels common to the family; never le or quantile
	Points []DistributionPoint // sorted by Bound ascending, +Inf last
	Sum    *MetricSeries       // nil until the exporter reports a sum
	Count  *MetricSeries       // nil until the exporter reports a count
}

// DistributionPoint is one bucket of a histogram or one quantile of a summary.
type DistributionPoint struct {
	// Bound is the bucket upper bound ("le") for histograms, the quantile for
	// summaries.
	Bound float64
	// Series holds the bucket's cumulative counts, or the quantile's values,
	// one per scrape.
	Series *MetricSeries
}

// findPoint returns the series recorded for bound, or nil if bound is new.
func (d *DistributionSeries) findPoint(bound float64) *MetricSeries {
	idx := sort.Search(len(d.Points), func(i int) bool { return d.Points[i].Bound >= bound })
	if idx < len(d.Points) && d.Points[idx].Bound == bound {
		return d.Points[idx].Series
	}
	return nil
}

// insertPoint adds bound in sorted position. +Inf lands last on its own.
func (d *DistributionSeries) insertPoint(bound float64, series *MetricSeries) {
	idx := sort.Search(len(d.Points), func(i int) bool { return d.Points[i].Bound >= bound })
	d.Points = append(d.Points, DistributionPoint{})
	copy(d.Points[idx+1:], d.Points[idx:])
	d.Points[idx] = DistributionPoint{Bound: bound, Series: series}
}

// eachSeries calls fn for every series held by the distribution, passing a key
// that is unique across the whole store. The key is structural rather than built
// from the display name, so a histogram "foo" and a real counter named
// "foo_count" cannot collide in the bookkeeping.
func (d *DistributionSeries) eachSeries(sig string, fn func(key string, series *MetricSeries)) {
	for _, point := range d.Points {
		fn(distPointKey(sig, point.Bound), point.Series)
	}
	if d.Sum != nil {
		fn(sig+"|sum", d.Sum)
	}
	if d.Count != nil {
		fn(sig+"|count", d.Count)
	}
}

func distPointKey(sig string, bound float64) string {
	return sig + "|point|" + formatBound(bound)
}

// formatBound renders a bucket bound or quantile the way the text format does,
// so "le=1" stays "1" rather than becoming "1.0".
func formatBound(bound float64) string {
	if math.IsInf(bound, +1) {
		return "+Inf"
	}
	return strconv.FormatFloat(bound, 'g', -1, 64)
}

// ScrapeInfo describes one successful scrape, which is one column of history.
type ScrapeInfo struct {
	Time time.Time
	// Epoch counts the connection breaks seen before this scrape. Scrapes either
	// side of a failed fetch land in different epochs: the target most likely
	// restarted in between, so its values are not continuous across the break.
	Epoch int
}

type Store struct {
	Metrics       map[string]*MetricSeries       // gauge, counter and untyped metrics
	Distributions map[string]*DistributionSeries // histogram and summary families
	HistoryLimit  int
	// Scrapes has one entry per retained column, oldest first. Every series gets
	// exactly one value per scrape and is pruned alongside, so a series' values
	// line up with the tail of this slice.
	Scrapes []ScrapeInfo

	epoch        int
	breakPending bool
}

func NewStore(historyLimit int) *Store {
	return &Store{
		Metrics:       make(map[string]*MetricSeries),
		Distributions: make(map[string]*DistributionSeries),
		HistoryLimit:  historyLimit,
	}
}

// GenerateSignature creates a unique key for a metric based on name and labels
func GenerateSignature(name string, labels map[string]string) string {
	// Sort label keys to ensure consistent signature
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf("%s=%q", k, labels[k]))
	}
	sb.WriteString("}")
	return sb.String()
}

// MarkBreak records that a scrape failed. The next successful scrape starts a
// new epoch; any number of failures in a row count as one break.
func (s *Store) MarkBreak() {
	s.breakPending = true
}

// ScrapeAt returns the scrape offset columns back from the newest (0 = newest).
func (s *Store) ScrapeAt(offset int) (ScrapeInfo, bool) {
	idx := len(s.Scrapes) - 1 - offset
	if offset < 0 || idx < 0 {
		return ScrapeInfo{}, false
	}
	return s.Scrapes[idx], true
}

// Epochs returns the epochs of the newest n scrapes, oldest first, in the
// right-aligned form ValuesWithDeltas takes. Fewer come back when fewer are
// retained.
func (s *Store) Epochs(n int) []int {
	if n > len(s.Scrapes) {
		n = len(s.Scrapes)
	}
	epochs := make([]int, n)
	for i, scrape := range s.Scrapes[len(s.Scrapes)-n:] {
		epochs[i] = scrape.Epoch
	}
	return epochs
}

// EpochParity is 0 for columns in the newest epoch and alternates 1, 0, 1 for
// each break further back, which is what the views shade by. Columns with no
// scrape behind them count as the newest epoch.
func (s *Store) EpochParity(offset int) int {
	scrape, ok := s.ScrapeAt(offset)
	if !ok {
		return 0
	}
	return (s.epoch - scrape.Epoch) % 2
}

// UpdateFromFamilies updates the store with a fresh batch of metrics scraped at
// time at. It handles appending new values and filling missing metrics with NaN.
func (s *Store) UpdateFromFamilies(families map[string]*dto.MetricFamily, at time.Time) {
	if s.breakPending && len(s.Scrapes) > 0 {
		s.epoch++
	}
	s.breakPending = false
	s.Scrapes = append(s.Scrapes, ScrapeInfo{Time: at, Epoch: s.epoch})
	if len(s.Scrapes) > s.HistoryLimit {
		s.Scrapes = s.Scrapes[1:]
	}

	seenSignatures := make(map[string]bool)

	for _, family := range families {
		name := family.GetName()
		for _, metric := range family.GetMetric() {
			labels := metricLabels(metric)

			var value float64
			var kind MetricKind
			switch {
			case metric.Gauge != nil:
				value, kind = metric.Gauge.GetValue(), KindGauge
			case metric.Counter != nil:
				value, kind = metric.Counter.GetValue(), KindCounter
			case metric.Untyped != nil:
				value, kind = metric.Untyped.GetValue(), KindUntyped
			case metric.Histogram != nil:
				s.updateDistribution(name, KindHistogram, labels, metric, seenSignatures)
				continue
			case metric.Summary != nil:
				s.updateDistribution(name, KindSummary, labels, metric, seenSignatures)
				continue
			default:
				continue
			}

			sig := GenerateSignature(name, labels)
			s.updateMetric(sig, name, kind, labels, value)
			seenSignatures[sig] = true
		}
	}

	// Handle missing metrics. Every series gets exactly one value per scrape so
	// that a position in Values always means the same scrape across all series.
	for sig, series := range s.Metrics {
		if !seenSignatures[sig] {
			s.appendValue(series, math.NaN())
		}
	}
	for sig, dist := range s.Distributions {
		dist.eachSeries(sig, func(key string, series *MetricSeries) {
			if !seenSignatures[key] {
				s.appendValue(series, math.NaN())
			}
		})
	}
}

func (s *Store) updateMetric(sig, name string, kind MetricKind, labels map[string]string, value float64) {
	series, exists := s.Metrics[sig]
	if !exists {
		series = s.newSeries(name, kind, labels)
		s.Metrics[sig] = series
	}
	s.appendValue(series, value)
}

// updateDistribution appends this scrape's samples for one histogram or summary
// metric, creating the family and any newly seen bucket or quantile on the way.
func (s *Store) updateDistribution(name string, kind MetricKind, labels map[string]string, metric *dto.Metric, seenSignatures map[string]bool) {
	sig := GenerateSignature(name, labels)

	dist, exists := s.Distributions[sig]
	if !exists {
		dist = &DistributionSeries{
			Name:   name,
			Kind:   kind,
			Labels: labels,
		}
		s.Distributions[sig] = dist
	}

	// Buckets are cumulative counters; quantiles are gauges sharing the family name.
	pointName, pointLabel, pointKind := name+"_bucket", "le", KindCounter
	if kind == KindSummary {
		pointName, pointLabel, pointKind = name, "quantile", KindGauge
	}

	for _, point := range distributionPoints(metric) {
		if math.IsNaN(point.bound) {
			// A NaN bound never compares equal to itself, so it would add a fresh
			// point on every scrape. Exporters should not emit one.
			continue
		}
		series := dist.findPoint(point.bound)
		if series == nil {
			series = s.newSeries(pointName, pointKind, withLabel(labels, pointLabel, formatBound(point.bound)))
			dist.insertPoint(point.bound, series)
		}
		s.appendValue(series, point.value)
		seenSignatures[distPointKey(sig, point.bound)] = true
	}

	if sum, ok := sampleSum(metric); ok {
		if dist.Sum == nil {
			dist.Sum = s.newSeries(name+"_sum", KindCounter, copyLabels(labels))
		}
		s.appendValue(dist.Sum, sum)
		seenSignatures[sig+"|sum"] = true
	}
	if count, ok := sampleCount(metric); ok {
		if dist.Count == nil {
			dist.Count = s.newSeries(name+"_count", KindCounter, copyLabels(labels))
		}
		s.appendValue(dist.Count, count)
		seenSignatures[sig+"|count"] = true
	}
}

func (s *Store) newSeries(name string, kind MetricKind, labels map[string]string) *MetricSeries {
	return &MetricSeries{
		Name:   name,
		Kind:   kind,
		Labels: labels,
		Values: make([]float64, 0, s.HistoryLimit),
	}
}

func (s *Store) appendValue(series *MetricSeries, value float64) {
	// Append new value
	series.Values = append(series.Values, value)

	// Prune if exceeding history limit
	if len(series.Values) > s.HistoryLimit {
		series.Values = series.Values[1:]
	}
}

// boundValue is one bound/value pair read out of a histogram bucket or a summary
// quantile.
type boundValue struct {
	bound float64
	value float64
}

// distributionPoints extracts the buckets of a histogram or the quantiles of a
// summary. A native (exponential) histogram carries no buckets and yields none.
func distributionPoints(metric *dto.Metric) []boundValue {
	if hist := metric.Histogram; hist != nil {
		points := make([]boundValue, 0, len(hist.GetBucket()))
		for _, bucket := range hist.GetBucket() {
			count := float64(bucket.GetCumulativeCount())
			if bucket.CumulativeCountFloat != nil {
				count = bucket.GetCumulativeCountFloat()
			}
			points = append(points, boundValue{bound: bucket.GetUpperBound(), value: count})
		}
		return points
	}
	if summary := metric.Summary; summary != nil {
		points := make([]boundValue, 0, len(summary.GetQuantile()))
		for _, quantile := range summary.GetQuantile() {
			points = append(points, boundValue{bound: quantile.GetQuantile(), value: quantile.GetValue()})
		}
		return points
	}
	return nil
}

// sampleSum reports the observation sum, and whether the exporter set it at all.
// The generated getters cannot distinguish an absent sum from a sum of zero.
func sampleSum(metric *dto.Metric) (float64, bool) {
	if hist := metric.Histogram; hist != nil {
		return hist.GetSampleSum(), hist.SampleSum != nil
	}
	if summary := metric.Summary; summary != nil {
		return summary.GetSampleSum(), summary.SampleSum != nil
	}
	return 0, false
}

// sampleCount reports the observation count, and whether the exporter set it.
func sampleCount(metric *dto.Metric) (float64, bool) {
	if hist := metric.Histogram; hist != nil {
		if hist.SampleCountFloat != nil {
			return hist.GetSampleCountFloat(), true
		}
		return float64(hist.GetSampleCount()), hist.SampleCount != nil
	}
	if summary := metric.Summary; summary != nil {
		return float64(summary.GetSampleCount()), summary.SampleCount != nil
	}
	return 0, false
}

func metricLabels(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}

func copyLabels(labels map[string]string) map[string]string {
	res := make(map[string]string, len(labels))
	for k, v := range labels {
		res[k] = v
	}
	return res
}

// withLabel copies labels and adds one. Every series owns its own label map, so
// the family's map is never shared with the buckets derived from it.
func withLabel(labels map[string]string, key, value string) map[string]string {
	res := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		res[k] = v
	}
	res[key] = value
	return res
}
