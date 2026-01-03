package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

type MetricSeries struct {
	Name   string
	Labels map[string]string
	Values []float64
}

// ValuesWithDeltas returns the values, optionally converting them to deltas.
// If deltas is true:
// - The last value is the absolute value.
// - Previous values are the delta to the next value (val[i+1] - val[i]).
func (s *MetricSeries) ValuesWithDeltas(deltas bool) []float64 {
	if !deltas {
		return s.Values
	}

	if len(s.Values) == 0 {
		return nil
	}

	res := make([]float64, len(s.Values))
	lastIdx := len(s.Values) - 1

	// Last element is absolute
	res[lastIdx] = s.Values[lastIdx]

	// Previous elements are deltas to the next element
	for i := 0; i < lastIdx; i++ {
		curr := s.Values[i]
		next := s.Values[i+1]
		if math.IsNaN(curr) || math.IsNaN(next) {
			res[i] = math.NaN()
		} else {
			res[i] = next - curr
		}
	}
	return res
}

type Store struct {
	Metrics      map[string]*MetricSeries
	HistoryLimit int
}

func NewStore(historyLimit int) *Store {
	return &Store{
		Metrics:      make(map[string]*MetricSeries),
		HistoryLimit: historyLimit,
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
			labels := make(map[string]string)
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}

			var value float64
			if metric.Gauge != nil {
				value = metric.Gauge.GetValue()
			} else if metric.Counter != nil {
				value = metric.Counter.GetValue()
			} else if metric.Untyped != nil {
				value = metric.Untyped.GetValue()
			} else {
				// Skip complex types for now
				continue
			}

			sig := GenerateSignature(name, labels)
			s.updateMetric(sig, name, labels, value)
			seenSignatures[sig] = true
		}
	}

	// Handle missing metrics
	for sig, series := range s.Metrics {
		if !seenSignatures[sig] {
			s.appendValue(series, math.NaN())
		}
	}
}

func (s *Store) updateMetric(sig, name string, labels map[string]string, value float64) {
	series, exists := s.Metrics[sig]
	if !exists {
		series = &MetricSeries{
			Name:   name,
			Labels: labels,
			Values: make([]float64, 0, s.HistoryLimit),
		}
		s.Metrics[sig] = series
	}
	s.appendValue(series, value)
}

func (s *Store) appendValue(series *MetricSeries, value float64) {
	// Append new value
	series.Values = append(series.Values, value)

	// Prune if exceeding history limit
	if len(series.Values) > s.HistoryLimit {
		series.Values = series.Values[1:]
	}
}
