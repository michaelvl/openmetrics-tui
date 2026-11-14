// Package testutil holds fixtures shared by the tests of several packages.
package testutil

import (
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	promModel "github.com/prometheus/common/model"
)

// ParseFamilies turns an exposition text fixture into the families the fetcher
// hands to the store.
func ParseFamilies(t *testing.T, text string) map[string]*dto.MetricFamily {
	t.Helper()
	parser := expfmt.NewTextParser(promModel.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return families
}

const HistogramFixture = `# TYPE requests_in_flight gauge
requests_in_flight 7
# TYPE requests_total counter
requests_total 1500
# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{method="GET",le="0.05"} 1203
request_duration_seconds_bucket{method="GET",le="1"} 4991
request_duration_seconds_bucket{method="GET",le="+Inf"} 5000
request_duration_seconds_sum{method="GET"} 612.44
request_duration_seconds_count{method="GET"} 5000
`

const SummaryFixture = `# TYPE rpc_seconds summary
rpc_seconds{quantile="0.5"} 0.031
rpc_seconds{quantile="0.9"} 0.24
rpc_seconds{quantile="0.99"} 0.51
rpc_seconds_sum 12.4
rpc_seconds_count 400
`

// KeysOf returns the keys of a map, for failure messages.
func KeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

const LatencyFixture = `# TYPE lat_seconds histogram
lat_seconds_bucket{le="0.1"} 20
lat_seconds_bucket{le="0.5"} 60
lat_seconds_bucket{le="1"} 90
lat_seconds_bucket{le="+Inf"} 100
lat_seconds_sum 45
lat_seconds_count 100
`
