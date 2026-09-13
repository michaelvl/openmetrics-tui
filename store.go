package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

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
// - "view": All values are deltas; historical same as "next", current is (last_historical - first_historical)
//
// A counter that goes backwards has been reset, and the drop is an artefact of
// the restart rather than something that was measured, so it is reported as
// missing rather than as a large negative number. A gauge is free to fall -
// that is what a gauge is for - so the guard is keyed on the series kind.
func (s *MetricSeries) ValuesWithDeltas(mode string) []float64 {
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
		if math.IsNaN(curr) || math.IsNaN(next) || s.isReset(curr, next) {
			res[i] = math.NaN()
		} else {
			res[i] = next - curr
		}
	}

	// Handle the current/last value based on mode
	if mode == "view" {
		// In "view" mode, current shows diff between first and last historical
		// Find first and last non-NaN historical values
		firstHistIdx := -1
		lastHistIdx := -1

		for i := 0; i < lastIdx; i++ {
			if !math.IsNaN(s.Values[i]) {
				if firstHistIdx == -1 {
					firstHistIdx = i
				}
				lastHistIdx = i
			}
		}

		if firstHistIdx != -1 && lastHistIdx != -1 && firstHistIdx != lastHistIdx &&
			!s.isReset(s.Values[firstHistIdx], s.Values[lastHistIdx]) {
			res[lastIdx] = s.Values[lastHistIdx] - s.Values[firstHistIdx]
		} else {
			// Not enough historical data for a view delta, or a reset inside the
			// window that would make the span meaningless.
			res[lastIdx] = math.NaN()
		}
	} else {
		// In "next" mode, last element is absolute
		res[lastIdx] = s.Values[lastIdx]
	}

	return res
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

type Store struct {
	Metrics       map[string]*MetricSeries       // gauge, counter and untyped metrics
	Distributions map[string]*DistributionSeries // histogram and summary families
	HistoryLimit  int
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

// UpdateFromFamilies updates the store with a fresh batch of metrics.
// It handles appending new values and filling missing metrics with NaN.
func (s *Store) UpdateFromFamilies(families map[string]*dto.MetricFamily) {
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
