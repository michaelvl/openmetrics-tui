package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashapp"
	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

var fixedNow = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	newPromAPI(func() time.Time { return fixedNow }).register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type apiResult struct {
	Status    string          `json:"status"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

func post(t *testing.T, srv *httptest.Server, path string, form url.Values) (int, apiResult) {
	t.Helper()
	resp, err := http.PostForm(srv.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r apiResult
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return resp.StatusCode, r
}

const reconcileQuery = `sum by (controller) (rate(controller_runtime_reconcile_total{namespace="prod", controller=~".*"}[5m])) * 60
/
on (controller) group_left
count by (controller) (kube_customresource_info{namespace="prod", controller=~".*"})`

func rangeForm(q string, start, end time.Time, step string) url.Values {
	return url.Values{
		"query": {q},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {step},
	}
}

type matrix struct {
	ResultType string `json:"resultType"`
	Result     []struct {
		Metric map[string]string `json:"metric"`
		Values [][2]any          `json:"values"`
	} `json:"result"`
}

func TestQueryRangeHonoursStartEndStep(t *testing.T) {
	srv := newTestServer(t)
	start := fixedNow.Add(-time.Hour)
	code, r := post(t, srv, "/api/v1/query_range", rangeForm(reconcileQuery, start, fixedNow, "60"))
	if code != 200 || r.Status != "success" {
		t.Fatalf("got %d %+v", code, r)
	}
	var m matrix
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatal(err)
	}
	if m.ResultType != "matrix" || len(m.Result) != 4 {
		t.Fatalf("resultType %q, %d series", m.ResultType, len(m.Result))
	}
	for _, s := range m.Result {
		if len(s.Values) != 61 {
			t.Fatalf("%v: %d points, want 61", s.Metric, len(s.Values))
		}
		if first := s.Values[0][0].(float64); first != float64(start.Unix()) {
			t.Fatalf("first timestamp %v, want %d", first, start.Unix())
		}
		if gap := s.Values[1][0].(float64) - s.Values[0][0].(float64); gap != 60 {
			t.Fatalf("step %v, want 60", gap)
		}
	}
}

func lastValue(t *testing.T, m matrix, controller string) float64 {
	t.Helper()
	for _, s := range m.Result {
		if s.Metric["controller"] == controller {
			v, err := strconv.ParseFloat(s.Values[len(s.Values)-1][1].(string), 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("no series for %s", controller)
	return 0
}

func firstValue(t *testing.T, m matrix, controller string) float64 {
	t.Helper()
	for _, s := range m.Result {
		if s.Metric["controller"] == controller {
			v, _ := strconv.ParseFloat(s.Values[0][1].(string), 64)
			return v
		}
	}
	t.Fatalf("no series for %s", controller)
	return 0
}

func TestReconcileTrends(t *testing.T) {
	srv := newTestServer(t)
	_, r := post(t, srv, "/api/v1/query_range", rangeForm(reconcileQuery, fixedNow.Add(-time.Hour), fixedNow, "60"))
	var m matrix
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatal(err)
	}
	if g0, g1 := firstValue(t, m, "gadget-controller"), lastValue(t, m, "gadget-controller"); g1 < 3 || g0 > 1.5 {
		t.Errorf("gadget-controller should rise from ~1 to ~3.2, got %v -> %v", g0, g1)
	}
	if d := lastValue(t, m, "database-controller"); d < 5 {
		t.Errorf("database-controller should be high, got %v", d)
	}
	if i := lastValue(t, m, "ingress-controller"); i > 0.2 {
		t.Errorf("ingress-controller should be flat near zero, got %v", i)
	}
}

func TestControllerMatcherFilters(t *testing.T) {
	srv := newTestServer(t)
	q := strings.ReplaceAll(reconcileQuery, `controller=~".*"`, `controller=~"gad.*"`)
	_, r := post(t, srv, "/api/v1/query_range", rangeForm(q, fixedNow.Add(-time.Minute), fixedNow, "30"))
	var m matrix
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Result) != 1 || m.Result[0].Metric["controller"] != "gadget-controller" {
		t.Fatalf("got %+v", m.Result)
	}
}

func TestInstantQueryReturnsVector(t *testing.T) {
	srv := newTestServer(t)
	_, r := post(t, srv, "/api/v1/query", url.Values{
		"query": {`max by (name) (workqueue_depth{namespace="prod"})`},
	})
	var v struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(r.Data, &v); err != nil {
		t.Fatal(err)
	}
	if v.ResultType != "vector" || len(v.Result) != 2 {
		t.Fatalf("got %+v", v)
	}
	for _, s := range v.Result {
		if d, _ := strconv.ParseFloat(s.Value[1].(string), 64); s.Metric["name"] == "gadget" && (d < 135 || d > 150) {
			t.Errorf("gadget queue depth %v, want about 143", d)
		}
	}
}

func TestUnknownQueryAndNamespaceAreEmpty(t *testing.T) {
	srv := newTestServer(t)
	for _, q := range []string{
		`up`,
		`max by (name) (workqueue_depth{namespace="nowhere"})`,
	} {
		code, r := post(t, srv, "/api/v1/query_range", rangeForm(q, fixedNow.Add(-time.Minute), fixedNow, "15"))
		var m matrix
		if err := json.Unmarshal(r.Data, &m); err != nil {
			t.Fatal(err)
		}
		if code != 200 || r.Status != "success" || m.ResultType != "matrix" || len(m.Result) != 0 {
			t.Errorf("%q: got %d %+v %+v", q, code, r, m)
		}
	}
}

func TestBadParameters(t *testing.T) {
	srv := newTestServer(t)
	ok := rangeForm(`up`, fixedNow.Add(-time.Minute), fixedNow, "15")
	cases := map[string]func(url.Values){
		"no query":    func(f url.Values) { f.Del("query") },
		"bad start":   func(f url.Values) { f.Set("start", "yesterday") },
		"zero step":   func(f url.Values) { f.Set("step", "0") },
		"end < start": func(f url.Values) { f.Set("end", strconv.FormatInt(fixedNow.Add(-time.Hour).Unix(), 10)) },
		"too many": func(f url.Values) {
			f.Set("step", "1")
			f.Set("start", strconv.FormatInt(fixedNow.Add(-4*time.Hour).Unix(), 10))
		},
		"bad regex":     func(f url.Values) { f.Set("query", strings.ReplaceAll(reconcileQuery, `".*"`, `"("`)) },
		"duration step": func(f url.Values) { f.Set("step", "15s") }, // valid, must not fail
	}
	for name, mutate := range cases {
		f := url.Values{}
		for k, v := range ok {
			f[k] = v
		}
		mutate(f)
		code, r := post(t, srv, "/api/v1/query_range", f)
		if name == "duration step" {
			if code != 200 {
				t.Errorf("%s: got %d %+v", name, code, r)
			}
			continue
		}
		if code != 400 || r.Status != "error" || r.ErrorType != "bad_data" || r.Error == "" {
			t.Errorf("%s: got %d %+v", name, code, r)
		}
	}
}

func TestLabelValues(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/v1/label/controller/values?match[]=up")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r apiResult
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	var vals []string
	if err := json.Unmarshal(r.Data, &vals); err != nil {
		t.Fatal(err)
	}
	if r.Status != "success" || len(vals) != 4 || vals[0] != "database-controller" {
		t.Fatalf("got %+v %v", r, vals)
	}
	_, r = post(t, srv, "/api/v1/label/nosuch/values", nil)
	if string(r.Data) != "[]" {
		t.Fatalf("unknown label: got %s", r.Data)
	}
}

// The example dashboard is what the demo and the screenshots run, so it has to
// keep validating and keep getting rows from every panel.
func TestExampleDashboardChecksAgainstMock(t *testing.T) {
	d, err := dashboard.Load("../../examples/controllers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	vals, err := d.Values(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t)
	c, err := promapi.New(promapi.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if failed := dashapp.Check(context.Background(), d, vals, c, fixedNow, &out); failed != 0 {
		t.Fatalf("%d panels failed:\n%s", failed, out.String())
	}
	if strings.Contains(out.String(), "no data") || strings.Count(out.String(), "ok ") != len(d.Panels) {
		t.Fatalf("every panel should return rows:\n%s", out.String())
	}
}
