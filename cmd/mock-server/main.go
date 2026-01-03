package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

type MetricsState struct {
	mu sync.Mutex

	// Counters
	httpRequests200 float64
	httpRequests400 float64

	// Histogram
	histBuckets []float64 // 0.05, 0.1, 0.2, 0.5, 1, +Inf
	histSum     float64
	histCount   float64

	// Summary
	rpcQuantiles map[float64]float64 // 0.01, 0.05, 0.5, 0.9, 0.99
	rpcSum       float64
	rpcCount     float64

	// Gauge
	memoryUsage float64
}

func NewMetricsState() *MetricsState {
	return &MetricsState{
		httpRequests200: 1027,
		httpRequests400: 3,
		histBuckets:     []float64{24054, 33444, 100392, 129389, 133988, 144320},
		histSum:         53423,
		histCount:       144320,
		rpcQuantiles: map[float64]float64{
			0.01: 3102,
			0.05: 3272,
			0.5:  4773,
			0.9:  9001,
			0.99: 76656,
		},
		rpcSum:      1.7560473e+07,
		rpcCount:    2693,
		memoryUsage: 1024 * 1024 * 512, // 512MB start
	}
}

func (s *MetricsState) Update() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update Counters
	s.httpRequests200 += float64(rand.Intn(5))
	if rand.Float64() < 0.1 {
		s.httpRequests400 += 1
	}

	// Update Histogram
	// Simulate a request duration
	duration := rand.Float64() * 1.2 // 0 to 1.2s
	s.histSum += duration
	s.histCount++

	// Update buckets
	// Buckets: 0.05, 0.1, 0.2, 0.5, 1, +Inf
	thresholds := []float64{0.05, 0.1, 0.2, 0.5, 1.0}
	for i, threshold := range thresholds {
		if duration <= threshold {
			s.histBuckets[i]++
		}
	}
	s.histBuckets[5]++ // +Inf always increments

	// Update Summary
	// Just wiggle the quantiles
	for k := range s.rpcQuantiles {
		change := (rand.Float64() - 0.5) * 100
		s.rpcQuantiles[k] += change
	}
	s.rpcCount++
	s.rpcSum += 5000 + (rand.Float64()-0.5)*1000

	// Update Gauge
	change := (rand.Float64() - 0.5) * 1024 * 1024 * 10 // +/- 5MB
	s.memoryUsage += change
	if s.memoryUsage < 0 {
		s.memoryUsage = 0
	}
}

func (s *MetricsState) Write(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()

	timestamp := time.Now().UnixMilli()

	fmt.Fprintln(w, "# HELP http_requests_total The total number of HTTP requests.")
	fmt.Fprintln(w, "# TYPE http_requests_total counter")
	fmt.Fprintf(w, "http_requests_total{method=\"post\",code=\"200\"} %.0f %d\n", s.httpRequests200, timestamp)
	fmt.Fprintf(w, "http_requests_total{method=\"post\",code=\"400\"} %.0f %d\n", s.httpRequests400, timestamp)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# HELP http_request_duration_seconds A histogram of the request duration.")
	fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"0.05\"} %.0f\n", s.histBuckets[0])
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"0.1\"} %.0f\n", s.histBuckets[1])
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"0.2\"} %.0f\n", s.histBuckets[2])
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"0.5\"} %.0f\n", s.histBuckets[3])
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"1\"} %.0f\n", s.histBuckets[4])
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"+Inf\"} %.0f\n", s.histBuckets[5])
	fmt.Fprintf(w, "http_request_duration_seconds_sum %.2f\n", s.histSum)
	fmt.Fprintf(w, "http_request_duration_seconds_count %.0f\n", s.histCount)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# HELP rpc_duration_seconds A summary of the RPC duration in seconds.")
	fmt.Fprintln(w, "# TYPE rpc_duration_seconds summary")
	fmt.Fprintf(w, "rpc_duration_seconds{quantile=\"0.01\"} %.2f\n", s.rpcQuantiles[0.01])
	fmt.Fprintf(w, "rpc_duration_seconds{quantile=\"0.05\"} %.2f\n", s.rpcQuantiles[0.05])
	fmt.Fprintf(w, "rpc_duration_seconds{quantile=\"0.5\"} %.2f\n", s.rpcQuantiles[0.5])
	fmt.Fprintf(w, "rpc_duration_seconds{quantile=\"0.9\"} %.2f\n", s.rpcQuantiles[0.9])
	fmt.Fprintf(w, "rpc_duration_seconds{quantile=\"0.99\"} %.2f\n", s.rpcQuantiles[0.99])
	fmt.Fprintf(w, "rpc_duration_seconds_sum %.2f\n", s.rpcSum)
	fmt.Fprintf(w, "rpc_duration_seconds_count %.0f\n", s.rpcCount)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# HELP memory_usage_bytes Current memory usage in bytes.")
	fmt.Fprintln(w, "# TYPE memory_usage_bytes gauge")
	fmt.Fprintf(w, "memory_usage_bytes %.0f %d\n", s.memoryUsage, timestamp)
}

func main() {
	port := flag.Int("port", 8080, "Port to run mock server on")
	flag.Parse()

	state := NewMetricsState()

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		state.Update()
		state.Write(w)
	})
	fmt.Printf("Starting mock server on :%d\n", *port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), nil); err != nil {
		fmt.Printf("Error starting server: %v\n", err)
	}
}
