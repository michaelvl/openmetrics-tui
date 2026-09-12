package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// serveText exposes a fixture as classic Prometheus text.
func serveText(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "text/plain") {
			t.Errorf("Accept header does not offer text: %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", string(expfmt.NewFormat(expfmt.TypeTextPlain)))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveProto re-encodes a fixture as delimited protobuf, the format only content
// negotiation can reach.
func serveProto(t *testing.T, families map[string]*dto.MetricFamily) *httptest.Server {
	t.Helper()
	format := expfmt.NewFormat(expfmt.TypeProtoDelim)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), expfmt.ProtoType) {
			t.Errorf("Accept header does not offer protobuf: %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", string(format))
		enc := expfmt.NewEncoder(w, format)
		for _, family := range families {
			if err := enc.Encode(family); err != nil {
				t.Errorf("encoding %s: %v", family.GetName(), err)
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Both wire formats must land in the store identically - the store never sees the
// _bucket/_sum/_count name mangling that only the text format has.
func TestFetchReachesSameStoreOverTextAndProtobuf(t *testing.T) {
	fixture := parseFamilies(t, histogramFixture)

	stores := map[string]*Store{}
	for name, url := range map[string]string{
		"text":  serveText(t, histogramFixture).URL,
		"proto": serveProto(t, fixture).URL,
	} {
		families, err := NewFetcher(url).Fetch()
		if err != nil {
			t.Fatalf("%s: Fetch: %v", name, err)
		}
		store := NewStore(10)
		store.UpdateFromFamilies(families)
		stores[name] = store
	}

	text, proto := stores["text"], stores["proto"]
	if len(text.Metrics) != len(proto.Metrics) || len(text.Distributions) != len(proto.Distributions) {
		t.Fatalf("text has %d/%d metrics/distributions, proto has %d/%d",
			len(text.Metrics), len(text.Distributions), len(proto.Metrics), len(proto.Distributions))
	}

	sig := `request_duration_seconds{method="GET"}`
	textDist, protoDist := text.Distributions[sig], proto.Distributions[sig]
	if textDist == nil || protoDist == nil {
		t.Fatalf("histogram missing: text=%v proto=%v", keysOf(text.Distributions), keysOf(proto.Distributions))
	}
	if len(textDist.Points) != len(protoDist.Points) {
		t.Fatalf("bucket counts differ: %v vs %v", boundsOf(textDist), boundsOf(protoDist))
	}
	for i := range textDist.Points {
		want, got := textDist.Points[i], protoDist.Points[i]
		if want.Bound != got.Bound || want.Series.Values[0] != got.Series.Values[0] {
			t.Errorf("point %d: text {%v: %v} vs proto {%v: %v}",
				i, want.Bound, want.Series.Values, got.Bound, got.Series.Values)
		}
		if want.Series.Labels["le"] != got.Series.Labels["le"] {
			t.Errorf("point %d le: %q vs %q", i, want.Series.Labels["le"], got.Series.Labels["le"])
		}
	}
	if textDist.Sum.Values[0] != protoDist.Sum.Values[0] || textDist.Count.Values[0] != protoDist.Count.Values[0] {
		t.Errorf("sum/count differ: text %v/%v proto %v/%v",
			textDist.Sum.Values, textDist.Count.Values, protoDist.Sum.Values, protoDist.Count.Values)
	}
}

// A native histogram has no classic buckets. It must survive ingestion so the
// view can report it as unsupported rather than the fetch failing.
func TestNativeHistogramYieldsNoBuckets(t *testing.T) {
	schema, zeroThreshold, zeroCount := int32(3), 0.001, uint64(4)
	count, sum := uint64(90), 12.5
	native := map[string]*dto.MetricFamily{
		"latency_seconds": {
			Name: strPtr("latency_seconds"),
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
	}

	families, err := NewFetcher(serveProto(t, native).URL).Fetch()
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	store := NewStore(10)
	store.UpdateFromFamilies(families)

	dist := store.Distributions[`latency_seconds{}`]
	if dist == nil {
		t.Fatalf("native histogram dropped entirely, have %v", keysOf(store.Distributions))
	}
	if len(dist.Points) != 0 {
		t.Errorf("expected no classic buckets, got %v", boundsOf(dist))
	}
	if dist.Sum == nil || dist.Sum.Values[0] != sum {
		t.Errorf("sum = %+v, want %v", dist.Sum, sum)
	}
	if dist.Count == nil || dist.Count.Values[0] != float64(count) {
		t.Errorf("count = %+v, want %v", dist.Count, count)
	}
}

// Without a status check an error page is parsed as metrics and silently yields
// nothing, which looks like a healthy but empty endpoint.
func TestFetchRejectsErrorResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	if _, err := NewFetcher(srv.URL).Fetch(); err == nil {
		t.Fatal("expected an error for a 404 response")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should name the status, got %v", err)
	}
}

// The decoder defaults to legacy name validation; the fetcher must keep UTF-8
// names working as they did before content negotiation.
func TestFetchKeepsUTF8MetricNames(t *testing.T) {
	srv := serveText(t, "# TYPE \"requests.total\" counter\n{\"requests.total\"} 42\n")

	families, err := NewFetcher(srv.URL).Fetch()
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	store := NewStore(10)
	store.UpdateFromFamilies(families)

	series := store.Metrics[`requests.total{}`]
	if series == nil {
		t.Fatalf("dotted metric name rejected, have %v", keysOf(store.Metrics))
	}
	if series.Values[0] != 42 || math.IsNaN(series.Values[0]) {
		t.Errorf("value = %v, want 42", series.Values)
	}
}

func strPtr(s string) *string    { return &s }
func int32Ptr(v int32) *int32    { return &v }
func uint32Ptr(v uint32) *uint32 { return &v }
