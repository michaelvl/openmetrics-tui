package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file serves a small Prometheus-compatible query API, so omtui-dash can
// be developed, tested and screenshotted without a cluster. Only the queries
// of examples/controllers.yaml are known; anything else is a valid empty
// result. Values are a pure function of the sample's age, so the trends (a
// controller slowly degrading, a flat one) look the same whenever you run it.

// maxPoints mirrors Prometheus' limit on points per series for a range query.
const maxPoints = 11000

// series is one synthetic series: its labels and the value at a given age
// (seconds before the server's "now").
type series struct {
	labels map[string]string
	at     func(age float64) float64
}

// A query template is the example dashboard's query with ${namespace} and
// ${controller} left in place, and whitespace normalised.
type promQuery struct {
	template string
	series   []series
	// filterLabel is the label ${controller} restricts, empty if the query has none.
	filterLabel string
}

type promAPI struct {
	now     func() time.Time
	queries []compiledQuery
}

type compiledQuery struct {
	re *regexp.Regexp
	promQuery
}

func normalize(q string) string { return strings.Join(strings.Fields(q), " ") }

func compile(q promQuery) compiledQuery {
	quoted := regexp.QuoteMeta(normalize(q.template))
	// QuoteMeta escapes "$", "{" and "}".
	quoted = regexp.MustCompile(`\\\$\\\{(namespace|controller)\\\}`).ReplaceAllString(quoted, `(?P<$1>[^"]*)`)
	// A template that mentions a placeholder twice would need distinct names.
	return compiledQuery{re: regexp.MustCompile("^" + dedupeGroups(quoted) + "$"), promQuery: q}
}

// dedupeGroups renames repeated named groups, which Go's regexp rejects.
func dedupeGroups(s string) string {
	seen := map[string]int{}
	return regexp.MustCompile(`\(\?P<(namespace|controller)>`).ReplaceAllStringFunc(s, func(m string) string {
		name := m[4 : len(m)-1]
		seen[name]++
		if seen[name] == 1 {
			return m
		}
		return fmt.Sprintf("(?P<%s_%d>", name, seen[name])
	})
}

// newPromAPI builds the handler set. now is injectable for tests.
func newPromAPI(now func() time.Time) *promAPI {
	a := &promAPI{now: now}
	for _, q := range exampleQueries {
		a.queries = append(a.queries, compile(q))
	}
	return a
}

func (a *promAPI) register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/query_range", a.handleQueryRange)
	mux.HandleFunc("/api/v1/query", a.handleQuery)
	mux.HandleFunc("/api/v1/label/", a.handleLabelValues)
}

type apiResponse struct {
	Status    string `json:"status"`
	Data      any    `json:"data,omitempty"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func badData(w http.ResponseWriter, format string, args ...any) {
	writeJSON(w, http.StatusBadRequest, apiResponse{
		Status: "error", ErrorType: "bad_data", Error: fmt.Sprintf(format, args...),
	})
}

type resultSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values,omitempty"`
	Value  *[2]any           `json:"value,omitempty"`
}

type resultData struct {
	ResultType string         `json:"resultType"`
	Result     []resultSeries `json:"result"`
}

// parseTime accepts Prometheus' unix seconds (fractional) and RFC 3339.
func parseTime(s string) (time.Time, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.UnixMilli(int64(math.Round(f * 1000))), nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func parseStep(s string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	return time.ParseDuration(s)
}

func (a *promAPI) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		badData(w, "%v", err)
		return
	}
	query := r.Form.Get("query")
	if query == "" {
		badData(w, "invalid parameter %q: no query", "query")
		return
	}
	start, err := parseTime(r.Form.Get("start"))
	if err != nil {
		badData(w, "invalid parameter %q: cannot parse %q to a valid timestamp", "start", r.Form.Get("start"))
		return
	}
	end, err := parseTime(r.Form.Get("end"))
	if err != nil {
		badData(w, "invalid parameter %q: cannot parse %q to a valid timestamp", "end", r.Form.Get("end"))
		return
	}
	step, err := parseStep(r.Form.Get("step"))
	if err != nil || step <= 0 {
		badData(w, "invalid parameter %q: zero or negative query resolution step widths are not accepted. Try a positive integer", "step")
		return
	}
	if end.Before(start) {
		badData(w, "invalid parameter %q: end timestamp must not be before start time", "end")
		return
	}
	if end.Sub(start)/step > maxPoints {
		badData(w, "exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)")
		return
	}
	matched, err := a.lookup(query)
	if err != nil {
		badData(w, "%v", err)
		return
	}
	now := a.now()
	out := []resultSeries{}
	for _, s := range matched {
		rs := resultSeries{Metric: s.labels, Values: [][2]any{}}
		for t := start; !t.After(end); t = t.Add(step) {
			rs.Values = append(rs.Values, sample(s, t, now))
		}
		out = append(out, rs)
	}
	writeJSON(w, http.StatusOK, apiResponse{Status: "success", Data: resultData{ResultType: "matrix", Result: out}})
}

func (a *promAPI) handleQuery(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		badData(w, "%v", err)
		return
	}
	query := r.Form.Get("query")
	if query == "" {
		badData(w, "invalid parameter %q: no query", "query")
		return
	}
	now := a.now()
	at := now
	if ts := r.Form.Get("time"); ts != "" {
		var err error
		if at, err = parseTime(ts); err != nil {
			badData(w, "invalid parameter %q: cannot parse %q to a valid timestamp", "time", ts)
			return
		}
	}
	matched, err := a.lookup(query)
	if err != nil {
		badData(w, "%v", err)
		return
	}
	out := []resultSeries{}
	for _, s := range matched {
		v := sample(s, at, now)
		out = append(out, resultSeries{Metric: s.labels, Value: &v})
	}
	writeJSON(w, http.StatusOK, apiResponse{Status: "success", Data: resultData{ResultType: "vector", Result: out}})
}

func sample(s series, t, now time.Time) [2]any {
	age := now.Sub(t).Seconds()
	return [2]any{float64(t.UnixMilli()) / 1000, strconv.FormatFloat(s.at(age), 'g', 6, 64)}
}

// lookup returns the series for a known query, restricted by the namespace and
// controller matchers in it. Unknown queries, and namespaces the mock has no
// data for, give an empty result; an unparseable controller regex is an error.
func (a *promAPI) lookup(query string) ([]series, error) {
	q := normalize(query)
	for _, c := range a.queries {
		m := c.re.FindStringSubmatch(q)
		if m == nil {
			continue
		}
		var ns, ctrl string
		ctrlSet := false
		for i, name := range c.re.SubexpNames() {
			switch {
			case strings.HasPrefix(name, "namespace") && ns == "":
				ns = m[i]
			case strings.HasPrefix(name, "controller") && !ctrlSet:
				ctrl, ctrlSet = m[i], true
			}
		}
		if strings.Contains(c.template, "${namespace}") && !contains(namespaces, ns) {
			return nil, nil
		}
		var re *regexp.Regexp
		if ctrlSet {
			var err error
			// Selector regexes are fully anchored, as in PromQL.
			if re, err = regexp.Compile("^(?:" + unescapeString(ctrl) + ")$"); err != nil {
				return nil, fmt.Errorf("parse error: invalid regexp %q: %v", ctrl, err)
			}
		}
		var out []series
		for _, s := range c.series {
			if re != nil && !re.MatchString(s.labels[c.filterLabel]) {
				continue
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, nil
}

func unescapeString(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\x27`, `'`).Replace(s)
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// labelValues are what /api/v1/label/<name>/values serves.
var namespaces = []string{"prod", "staging"}

var labelValues = map[string][]string{
	"namespace":  namespaces,
	"controller": {"database-controller", "gadget-controller", "ingress-controller", "widget-controller"},
	"name":       {"gadget", "widget"},
	"pod":        {"database-0", "gadget-6f9c-x2k"},
}

func (a *promAPI) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/label/")
	name, ok := strings.CutSuffix(rest, "/values")
	if !ok || name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	name, err := url.PathUnescape(name)
	if err != nil {
		badData(w, "%v", err)
		return
	}
	vals := append([]string{}, labelValues[name]...)
	sort.Strings(vals)
	writeJSON(w, http.StatusOK, apiResponse{Status: "success", Data: vals})
}

// Shapes.

// noise is a deterministic wobble in [-1, 1] keyed by series and time, so the
// sparklines are not dead straight but repeat exactly between runs.
func noise(seed int, age float64) float64 {
	t := math.Floor(age / 15)
	return math.Sin(t*12.9898+float64(seed)*78.233) * math.Sin(t*0.37+float64(seed))
}

// ramp moves from `from` (an hour ago or earlier) to `to` (now) along a curve
// whose steepness grows with exp: above 1 the rise is late.
func ramp(age, from, to, exp float64) float64 {
	p := 1 - math.Min(math.Max(age/3600, 0), 1)
	return from + (to-from)*math.Pow(p, exp)
}

func positive(v float64) float64 { return math.Max(v, 0) }

func lbl(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

var exampleQueries = []promQuery{
	{
		template: `sum by (controller) (rate(controller_runtime_reconcile_total{namespace="${namespace}", controller=~"${controller}"}[5m])) * 60
/
on (controller) group_left
count by (controller) (kube_customresource_info{namespace="${namespace}", controller=~"${controller}"})`,
		filterLabel: "controller",
		series: []series{
			{lbl("controller", "widget-controller"), func(a float64) float64 { return positive(0.5 + 0.12*noise(1, a)) }},
			{lbl("controller", "gadget-controller"), func(a float64) float64 { return positive(ramp(a, 0.9, 3.2, 6) + 0.1*noise(2, a)) }},
			{lbl("controller", "database-controller"), func(a float64) float64 { return positive(ramp(a, 6.2, 7.9, 0.7) + 0.15*noise(3, a)) }},
			{lbl("controller", "ingress-controller"), func(a float64) float64 { return positive(0.02 + 0.02*noise(4, a)) }},
		},
	},
	{
		template: `sum by (controller) (rate(controller_runtime_reconcile_errors_total{namespace="${namespace}", controller=~"${controller}"}[5m]))
/ sum by (controller) (rate(controller_runtime_reconcile_total{namespace="${namespace}", controller=~"${controller}"}[5m]))`,
		filterLabel: "controller",
		series: []series{
			{lbl("controller", "widget-controller"), func(float64) float64 { return 0 }},
			{lbl("controller", "gadget-controller"), func(a float64) float64 { return positive(ramp(a, 0.001, 0.014, 3) + 0.0005*noise(5, a)) }},
			{lbl("controller", "database-controller"), func(a float64) float64 { return positive(0.087 + 0.004*noise(6, a) + 0.0008*(1-math.Min(a/3600, 1))) }},
			{lbl("controller", "ingress-controller"), func(float64) float64 { return 0 }},
		},
	},
	{
		template: `max by (name) (workqueue_depth{namespace="${namespace}"})`,
		series: []series{
			{lbl("name", "widget"), func(a float64) float64 { return math.Round(positive(1 + 1.2*noise(7, a))) }},
			{lbl("name", "gadget"), func(a float64) float64 { return math.Round(positive(ramp(a, 3, 143, 3.5) + 2*noise(8, a))) }},
		},
	},
	{
		template: `sum by (pod) (increase(kube_pod_container_status_restarts_total{namespace="${namespace}"}[1h]))`,
		series: []series{
			{lbl("pod", "database-0"), func(a float64) float64 { return staircase(a, 0, 3000, 1, 1000, 2, 400, 3) }},
			{lbl("pod", "gadget-6f9c-x2k"), func(a float64) float64 { return staircase(a, 0, 600, 1) }},
		},
	},
	{
		template: `histogram_quantile(0.99, sum by (le) (rate(apiserver_request_duration_seconds_bucket[5m])))`,
		series: []series{
			{lbl(), func(a float64) float64 { return 0.21 + 0.015*noise(9, a) }},
		},
	},
}

// staircase is a base value, then each (age, value) pair applies once the
// sample is newer than that age. Pairs are given oldest first.
func staircase(age, base float64, pairs ...float64) float64 {
	v := base
	for i := 0; i+1 < len(pairs); i += 2 {
		if age <= pairs[i] {
			v = pairs[i+1]
		}
	}
	return v
}
