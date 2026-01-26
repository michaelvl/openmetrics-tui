package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

type MetricsState struct {
	mu sync.Mutex

	// HTTP request counters by method, endpoint, and status code
	httpRequests map[string]float64 // key: "method:endpoint:code"

	// HTTP bytes counters
	httpRequestBytes  map[string]float64 // key: "method:endpoint"
	httpResponseBytes map[string]float64 // key: "method:endpoint:code"

	// WebSocket counters
	websocketMessages map[string]float64 // key: "direction:channel"

	// API error counters
	apiErrors map[string]float64 // key: "method:endpoint:error_type"

	// Active connections gauge
	httpConnectionsActive map[string]float64 // key: "method:endpoint"

	// WebSocket connections gauge
	websocketConnectionsActive map[string]float64 // key: "channel"

	// Rate limit remaining gauge
	apiRateLimitRemaining map[string]float64 // key: "endpoint:client_tier"

	// Server goroutines gauge
	httpServerGoroutines map[string]float64 // key: "handler"

	// Bandwidth usage gauge
	bandwidthUsageMbps map[string]float64 // key: "direction"

	// Request duration gauge (current slowest request)
	httpRequestDurationCurrent map[string]float64 // key: "method:endpoint"

	// Histogram (existing)
	histBuckets []float64
	histSum     float64
	histCount   float64

	// Summary (existing)
	rpcQuantiles map[float64]float64
	rpcSum       float64
	rpcCount     float64

	// Memory gauge (existing)
	memoryUsage float64

	// Internal counters for rate limit simulation
	rateLimitCounters map[string]int
}

func NewMetricsState() *MetricsState {
	s := &MetricsState{
		httpRequests:               make(map[string]float64),
		httpRequestBytes:           make(map[string]float64),
		httpResponseBytes:          make(map[string]float64),
		websocketMessages:          make(map[string]float64),
		apiErrors:                  make(map[string]float64),
		httpConnectionsActive:      make(map[string]float64),
		websocketConnectionsActive: make(map[string]float64),
		apiRateLimitRemaining:      make(map[string]float64),
		httpServerGoroutines:       make(map[string]float64),
		bandwidthUsageMbps:         make(map[string]float64),
		httpRequestDurationCurrent: make(map[string]float64),
		rateLimitCounters:          make(map[string]int),
		histBuckets:                []float64{24054, 33444, 100392, 129389, 133988, 144320},
		histSum:                    53423,
		histCount:                  144320,
		rpcQuantiles: map[float64]float64{
			0.01: 3102,
			0.05: 3272,
			0.5:  4773,
			0.9:  9001,
			0.99: 76656,
		},
		rpcSum:      1.7560473e+07,
		rpcCount:    2693,
		memoryUsage: 1024 * 1024 * 512, // 512MB
	}

	// Initialize HTTP request counters with realistic starting values
	methods := []string{"get", "post", "put", "delete", "patch"}
	endpoints := []string{"/api/users", "/api/products", "/api/orders", "/health"}
	codes := []string{"200", "201", "400", "401", "404", "500", "503"}

	for _, method := range methods {
		for _, endpoint := range endpoints {
			for _, code := range codes {
				key := fmt.Sprintf("%s:%s:%s", method, endpoint, code)
				// More requests for successful codes, more GETs
				multiplier := 1.0
				if method == "get" {
					multiplier = 3.0
				} else if method == "post" {
					multiplier = 1.5
				}
				if code == "200" || code == "201" {
					s.httpRequests[key] = float64(rand.Intn(5000)) * multiplier
				} else {
					s.httpRequests[key] = float64(rand.Intn(100)) * multiplier
				}
			}

			// Initialize byte counters
			reqKey := fmt.Sprintf("%s:%s", method, endpoint)
			s.httpRequestBytes[reqKey] = float64(rand.Intn(1000000))

			for _, code := range codes {
				respKey := fmt.Sprintf("%s:%s:%s", method, endpoint, code)
				s.httpResponseBytes[respKey] = float64(rand.Intn(5000000))
			}

			// Initialize active connections gauge
			s.httpConnectionsActive[reqKey] = float64(rand.Intn(10))

			// Initialize request duration gauge
			s.httpRequestDurationCurrent[reqKey] = 0.05 + rand.Float64()*0.2
		}
	}

	// Initialize WebSocket metrics
	channels := []string{"chat", "notifications", "updates"}
	for _, channel := range channels {
		s.websocketMessages[fmt.Sprintf("sent:%s", channel)] = float64(rand.Intn(10000))
		s.websocketMessages[fmt.Sprintf("received:%s", channel)] = float64(rand.Intn(10000))
		s.websocketConnectionsActive[channel] = float64(20 + rand.Intn(50))
	}

	// Initialize API error counters
	errorTypes := []string{"timeout", "validation", "internal"}
	for _, method := range methods {
		for _, endpoint := range endpoints {
			for _, errType := range errorTypes {
				key := fmt.Sprintf("%s:%s:%s", method, endpoint, errType)
				s.apiErrors[key] = float64(rand.Intn(50))
			}
		}
	}

	// Initialize rate limits
	tiers := []string{"free", "premium"}
	for _, endpoint := range endpoints {
		for _, tier := range tiers {
			key := fmt.Sprintf("%s:%s", endpoint, tier)
			if tier == "premium" {
				s.apiRateLimitRemaining[key] = float64(8000 + rand.Intn(2000))
			} else {
				s.apiRateLimitRemaining[key] = float64(800 + rand.Intn(200))
			}
		}
	}

	// Initialize server goroutines
	handlers := []string{"api", "static", "websocket"}
	for _, handler := range handlers {
		s.httpServerGoroutines[handler] = float64(50 + rand.Intn(50))
	}

	// Initialize bandwidth
	s.bandwidthUsageMbps["inbound"] = 5 + rand.Float64()*10
	s.bandwidthUsageMbps["outbound"] = 10 + rand.Float64()*20

	return s
}

func (s *MetricsState) Update() {
	s.mu.Lock()
	defer s.mu.Unlock()

	methods := []string{"get", "post", "put", "delete", "patch"}
	endpoints := []string{"/api/users", "/api/products", "/api/orders", "/health"}

	// Traffic distribution weights
	methodWeights := map[string]float64{
		"get":    0.60,
		"post":   0.20,
		"put":    0.10,
		"delete": 0.05,
		"patch":  0.05,
	}

	endpointWeights := map[string]float64{
		"/api/users":    0.40,
		"/api/products": 0.30,
		"/api/orders":   0.20,
		"/health":       0.10,
	}

	// Generate random requests
	numRequests := rand.Intn(5) + 1
	for i := 0; i < numRequests; i++ {
		// Pick random method and endpoint based on weights
		method := weightedChoice(methods, methodWeights)
		endpoint := weightedChoice(endpoints, endpointWeights)

		// Determine status code (85% success, 10% client error, 5% server error)
		var code string
		r := rand.Float64()
		if r < 0.85 {
			if method == "post" || method == "put" {
				code = "201"
			} else {
				code = "200"
			}
		} else if r < 0.95 {
			codes := []string{"400", "401", "404"}
			code = codes[rand.Intn(len(codes))]
		} else {
			codes := []string{"500", "503"}
			code = codes[rand.Intn(len(codes))]
		}

		// Update request counter
		key := fmt.Sprintf("%s:%s:%s", method, endpoint, code)
		s.httpRequests[key]++

		// Update byte counters
		reqKey := fmt.Sprintf("%s:%s", method, endpoint)
		if method == "post" || method == "put" || method == "patch" {
			s.httpRequestBytes[reqKey] += float64(500 + rand.Intn(5000))
		} else {
			s.httpRequestBytes[reqKey] += float64(100 + rand.Intn(500))
		}

		respKey := fmt.Sprintf("%s:%s:%s", method, endpoint, code)
		if method == "get" && (code == "200" || code == "201") {
			s.httpResponseBytes[respKey] += float64(1000 + rand.Intn(10000))
		} else {
			s.httpResponseBytes[respKey] += float64(200 + rand.Intn(1000))
		}

		// Occasionally generate errors
		if code == "500" || code == "503" || rand.Float64() < 0.05 {
			errorTypes := []string{"timeout", "validation", "internal"}
			errType := errorTypes[rand.Intn(len(errorTypes))]
			errKey := fmt.Sprintf("%s:%s:%s", method, endpoint, errType)
			s.apiErrors[errKey]++
		}
	}

	// Update active connections gauge (fluctuate)
	for key := range s.httpConnectionsActive {
		change := rand.Intn(5) - 2 // -2 to +2
		s.httpConnectionsActive[key] += float64(change)
		if s.httpConnectionsActive[key] < 0 {
			s.httpConnectionsActive[key] = 0
		}
		if s.httpConnectionsActive[key] > 50 {
			s.httpConnectionsActive[key] = 50
		}
	}

	// Update request duration gauge (wave pattern)
	for key := range s.httpRequestDurationCurrent {
		s.httpRequestDurationCurrent[key] = 0.01 + 0.3*math.Sin(float64(time.Now().Unix()%60)/10.0) + rand.Float64()*0.1
		if s.httpRequestDurationCurrent[key] < 0.01 {
			s.httpRequestDurationCurrent[key] = 0.01
		}
	}

	// Update WebSocket messages
	channels := []string{"chat", "notifications", "updates"}
	for _, channel := range channels {
		// Chat is more active
		multiplier := 1
		if channel == "chat" {
			multiplier = 5
		}
		s.websocketMessages[fmt.Sprintf("sent:%s", channel)] += float64(rand.Intn(10) * multiplier)
		s.websocketMessages[fmt.Sprintf("received:%s", channel)] += float64(rand.Intn(10) * multiplier)
	}

	// Update WebSocket connections (slowly vary)
	for channel := range s.websocketConnectionsActive {
		change := rand.Intn(5) - 2
		s.websocketConnectionsActive[channel] += float64(change)
		if s.websocketConnectionsActive[channel] < 10 {
			s.websocketConnectionsActive[channel] = 10
		}
		if s.websocketConnectionsActive[channel] > 150 {
			s.websocketConnectionsActive[channel] = 150
		}
	}

	// Update rate limits (decrease then reset periodically)
	for key := range s.apiRateLimitRemaining {
		s.rateLimitCounters[key]++
		// Consume rate limit
		s.apiRateLimitRemaining[key] -= float64(rand.Intn(50))

		// Reset every ~20 updates
		if s.rateLimitCounters[key] > 20 {
			s.rateLimitCounters[key] = 0
			if key[len(key)-7:] == "premium" {
				s.apiRateLimitRemaining[key] = 9000 + rand.Float64()*1000
			} else {
				s.apiRateLimitRemaining[key] = 900 + rand.Float64()*100
			}
		}

		// Prevent negative
		if s.apiRateLimitRemaining[key] < 0 {
			s.apiRateLimitRemaining[key] = 0
		}
	}

	// Update server goroutines (occasional spikes)
	for handler := range s.httpServerGoroutines {
		if rand.Float64() < 0.1 {
			// Spike
			s.httpServerGoroutines[handler] += float64(rand.Intn(50))
		} else {
			// Gradual decrease
			s.httpServerGoroutines[handler] -= float64(rand.Intn(5))
		}

		if s.httpServerGoroutines[handler] < 20 {
			s.httpServerGoroutines[handler] = 20 + float64(rand.Intn(30))
		}
		if s.httpServerGoroutines[handler] > 200 {
			s.httpServerGoroutines[handler] = 200
		}
	}

	// Update bandwidth (wave pattern)
	s.bandwidthUsageMbps["inbound"] = 10 + 15*math.Sin(float64(time.Now().Unix()%120)/20.0) + rand.Float64()*5
	s.bandwidthUsageMbps["outbound"] = 20 + 20*math.Sin(float64(time.Now().Unix()%120)/20.0) + rand.Float64()*10

	// Update existing histogram
	duration := rand.Float64() * 1.2
	s.histSum += duration
	s.histCount++
	thresholds := []float64{0.05, 0.1, 0.2, 0.5, 1.0}
	for i, threshold := range thresholds {
		if duration <= threshold {
			s.histBuckets[i]++
		}
	}
	s.histBuckets[5]++

	// Update existing summary
	for k := range s.rpcQuantiles {
		change := (rand.Float64() - 0.5) * 100
		s.rpcQuantiles[k] += change
	}
	s.rpcCount++
	s.rpcSum += 5000 + (rand.Float64()-0.5)*1000

	// Update existing memory gauge
	change := (rand.Float64() - 0.5) * 1024 * 1024 * 10
	s.memoryUsage += change
	if s.memoryUsage < 0 {
		s.memoryUsage = 0
	}
}

func weightedChoice(items []string, weights map[string]float64) string {
	r := rand.Float64()
	cumulative := 0.0
	for _, item := range items {
		cumulative += weights[item]
		if r <= cumulative {
			return item
		}
	}
	return items[len(items)-1]
}

func (s *MetricsState) Write(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()

	timestamp := time.Now().UnixMilli()

	// HTTP requests counter
	fmt.Fprintln(w, "# HELP http_requests_total The total number of HTTP requests.")
	fmt.Fprintln(w, "# TYPE http_requests_total counter")
	for key, value := range s.httpRequests {
		parts := parseKey(key, 3)
		if len(parts) == 3 {
			fmt.Fprintf(w, "http_requests_total{method=\"%s\",endpoint=\"%s\",code=\"%s\"} %.0f %d\n",
				parts[0], parts[1], parts[2], value, timestamp)
		}
	}
	fmt.Fprintln(w)

	// HTTP request bytes counter
	fmt.Fprintln(w, "# HELP http_request_bytes_total Total bytes received in HTTP requests.")
	fmt.Fprintln(w, "# TYPE http_request_bytes_total counter")
	for key, value := range s.httpRequestBytes {
		parts := parseKey(key, 2)
		if len(parts) == 2 {
			fmt.Fprintf(w, "http_request_bytes_total{method=\"%s\",endpoint=\"%s\"} %.0f %d\n",
				parts[0], parts[1], value, timestamp)
		}
	}
	fmt.Fprintln(w)

	// HTTP response bytes counter
	fmt.Fprintln(w, "# HELP http_response_bytes_total Total bytes sent in HTTP responses.")
	fmt.Fprintln(w, "# TYPE http_response_bytes_total counter")
	for key, value := range s.httpResponseBytes {
		parts := parseKey(key, 3)
		if len(parts) == 3 {
			fmt.Fprintf(w, "http_response_bytes_total{method=\"%s\",endpoint=\"%s\",code=\"%s\"} %.0f %d\n",
				parts[0], parts[1], parts[2], value, timestamp)
		}
	}
	fmt.Fprintln(w)

	// WebSocket messages counter
	fmt.Fprintln(w, "# HELP websocket_messages_total Total WebSocket messages.")
	fmt.Fprintln(w, "# TYPE websocket_messages_total counter")
	for key, value := range s.websocketMessages {
		parts := parseKey(key, 2)
		if len(parts) == 2 {
			fmt.Fprintf(w, "websocket_messages_total{direction=\"%s\",channel=\"%s\"} %.0f %d\n",
				parts[0], parts[1], value, timestamp)
		}
	}
	fmt.Fprintln(w)

	// API errors counter
	fmt.Fprintln(w, "# HELP api_errors_total Total API errors by type.")
	fmt.Fprintln(w, "# TYPE api_errors_total counter")
	for key, value := range s.apiErrors {
		parts := parseKey(key, 3)
		if len(parts) == 3 {
			fmt.Fprintf(w, "api_errors_total{method=\"%s\",endpoint=\"%s\",error_type=\"%s\"} %.0f %d\n",
				parts[0], parts[1], parts[2], value, timestamp)
		}
	}
	fmt.Fprintln(w)

	// HTTP active connections gauge
	fmt.Fprintln(w, "# HELP http_connections_active Currently active HTTP connections.")
	fmt.Fprintln(w, "# TYPE http_connections_active gauge")
	for key, value := range s.httpConnectionsActive {
		parts := parseKey(key, 2)
		if len(parts) == 2 {
			fmt.Fprintf(w, "http_connections_active{method=\"%s\",endpoint=\"%s\"} %.0f %d\n",
				parts[0], parts[1], value, timestamp)
		}
	}
	fmt.Fprintln(w)

	// HTTP request duration current gauge
	fmt.Fprintln(w, "# HELP http_request_duration_current Current request duration in seconds.")
	fmt.Fprintln(w, "# TYPE http_request_duration_current gauge")
	for key, value := range s.httpRequestDurationCurrent {
		parts := parseKey(key, 2)
		if len(parts) == 2 {
			fmt.Fprintf(w, "http_request_duration_current{method=\"%s\",endpoint=\"%s\"} %.3f %d\n",
				parts[0], parts[1], value, timestamp)
		}
	}
	fmt.Fprintln(w)

	// WebSocket connections gauge
	fmt.Fprintln(w, "# HELP websocket_connections_active Currently active WebSocket connections.")
	fmt.Fprintln(w, "# TYPE websocket_connections_active gauge")
	for channel, value := range s.websocketConnectionsActive {
		fmt.Fprintf(w, "websocket_connections_active{channel=\"%s\"} %.0f %d\n",
			channel, value, timestamp)
	}
	fmt.Fprintln(w)

	// API rate limit gauge
	fmt.Fprintln(w, "# HELP api_rate_limit_remaining Remaining API rate limit capacity.")
	fmt.Fprintln(w, "# TYPE api_rate_limit_remaining gauge")
	for key, value := range s.apiRateLimitRemaining {
		parts := parseKey(key, 2)
		if len(parts) == 2 {
			fmt.Fprintf(w, "api_rate_limit_remaining{endpoint=\"%s\",client_tier=\"%s\"} %.0f %d\n",
				parts[0], parts[1], value, timestamp)
		}
	}
	fmt.Fprintln(w)

	// Server goroutines gauge
	fmt.Fprintln(w, "# HELP http_server_goroutines Active server goroutines by handler.")
	fmt.Fprintln(w, "# TYPE http_server_goroutines gauge")
	for handler, value := range s.httpServerGoroutines {
		fmt.Fprintf(w, "http_server_goroutines{handler=\"%s\"} %.0f %d\n",
			handler, value, timestamp)
	}
	fmt.Fprintln(w)

	// Bandwidth gauge
	fmt.Fprintln(w, "# HELP bandwidth_usage_mbps Current bandwidth usage in Mbps.")
	fmt.Fprintln(w, "# TYPE bandwidth_usage_mbps gauge")
	for direction, value := range s.bandwidthUsageMbps {
		fmt.Fprintf(w, "bandwidth_usage_mbps{direction=\"%s\"} %.2f %d\n",
			direction, value, timestamp)
	}
	fmt.Fprintln(w)

	// Existing histogram
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

	// Existing summary
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

	// Existing memory gauge
	fmt.Fprintln(w, "# HELP memory_usage_bytes Current memory usage in bytes.")
	fmt.Fprintln(w, "# TYPE memory_usage_bytes gauge")
	fmt.Fprintf(w, "memory_usage_bytes %.0f %d\n", s.memoryUsage, timestamp)
}

func parseKey(key string, expectedParts int) []string {
	parts := make([]string, 0, expectedParts)
	current := ""
	for _, ch := range key {
		if ch == ':' {
			parts = append(parts, current)
			current = ""
		} else {
			current += string(ch)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
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
	fmt.Printf("Try: curl http://localhost:%d/metrics\n", *port)
	fmt.Printf("Or:  ./openmetrics-tui -url http://localhost:%d/metrics -filter-label method=get\n", *port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), nil); err != nil {
		fmt.Printf("Error starting server: %v\n", err)
	}
}
