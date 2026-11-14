package promapi

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc, mod func(*Options)) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts := Options{URL: srv.URL}
	if mod != nil {
		mod(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func serve(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQueryRangeSuccess(t *testing.T) {
	var gotPath, gotCT string
	var gotForm map[string]string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		gotForm = map[string]string{
			"query": r.PostForm.Get("query"), "start": r.PostForm.Get("start"),
			"end": r.PostForm.Get("end"), "step": r.PostForm.Get("step"),
		}
		serve(200, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"job":"a"},"values":[[1700000000,"1"],[1700000015.5,"2.5"]]}]}}`)(w, r)
	}, nil)

	start := time.Unix(1700000000, 0)
	got, err := c.QueryRange(context.Background(), `up{job="a"}`, start, start.Add(time.Minute), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/query_range" || gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("path %q content-type %q", gotPath, gotCT)
	}
	want := map[string]string{"query": `up{job="a"}`, "start": "1700000000", "end": "1700000060", "step": "15"}
	for k, v := range want {
		if gotForm[k] != v {
			t.Errorf("form %s = %q, want %q", k, gotForm[k], v)
		}
	}
	if len(got) != 1 || got[0].Labels["job"] != "a" || len(got[0].Points) != 2 {
		t.Fatalf("unexpected result %+v", got)
	}
	p := got[0].Points[1]
	if !p.T.Equal(time.UnixMilli(1700000015500)) || p.V != 2.5 {
		t.Errorf("point = %+v", p)
	}
}

func TestInstantVector(t *testing.T) {
	var gotPath, gotTime string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = r.ParseForm()
		gotTime = r.PostForm.Get("time")
		serve(200, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"pod":"x"},"value":[1700000000,"7"]},
			{"metric":{"pod":"y"},"value":[1700000000,"8"]}]}}`)(w, r)
	}, nil)

	got, err := c.Instant(context.Background(), "up", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/query" || gotTime != "1700000000" {
		t.Errorf("path %q time %q", gotPath, gotTime)
	}
	if len(got) != 2 || got[1].Labels["pod"] != "y" || got[1].Points[0].V != 8 {
		t.Fatalf("unexpected result %+v", got)
	}
}

func TestInstantScalar(t *testing.T) {
	c := newTestClient(t, serve(200, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"42"]}}`), nil)
	got, err := c.Instant(context.Background(), "scalar(up)", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Labels) != 0 || got[0].Points[0].V != 42 {
		t.Fatalf("unexpected result %+v", got)
	}
}

func TestSpecialValues(t *testing.T) {
	c := newTestClient(t, serve(200, `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{},"values":[[1,"NaN"],[2,"+Inf"],[3,"-Inf"]]}]}}`), nil)
	got, err := c.QueryRange(context.Background(), "x", time.Unix(0, 0), time.Unix(3, 0), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pts := got[0].Points
	if !math.IsNaN(pts[0].V) || !math.IsInf(pts[1].V, 1) || !math.IsInf(pts[2].V, -1) {
		t.Errorf("points = %+v", pts)
	}
}

func TestEmptyResult(t *testing.T) {
	c := newTestClient(t, serve(200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`), nil)
	got, err := c.Instant(context.Background(), "x", time.Now())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestErrorPayload(t *testing.T) {
	// A 200 with status:error, and the usual 400 with the same body.
	body := `{"status":"error","errorType":"bad_data","error":"parse error: unexpected end of input"}`
	for _, code := range []int{200, 400} {
		c := newTestClient(t, serve(code, body), nil)
		_, err := c.Instant(context.Background(), "sum(", time.Now())
		if err == nil || !strings.Contains(err.Error(), "bad_data") || !strings.Contains(err.Error(), "parse error") {
			t.Errorf("status %d: err = %v", code, err)
		}
	}
}

func TestNon2xx(t *testing.T) {
	c := newTestClient(t, serve(502, "upstream down"), nil)
	_, err := c.Instant(context.Background(), "up", time.Now())
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream down") {
		t.Errorf("err = %v", err)
	}
}

func TestMalformedJSON(t *testing.T) {
	for _, body := range []string{
		`{not json`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"abc"]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{}}]}}`,
		`{"status":"success","data":{"resultType":"string","result":[1,"x"]}}`,
	} {
		c := newTestClient(t, serve(200, body), nil)
		if _, err := c.Instant(context.Background(), "up", time.Now()); err == nil {
			t.Errorf("expected error for %s", body)
		}
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}, func(o *Options) { o.Timeout = 50 * time.Millisecond })
	defer close(release)

	start := time.Now()
	_, err := c.Instant(context.Background(), "up", time.Now())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}
}

func TestContextCancel(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}, nil)
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	_, err := c.Instant(ctx, "up", time.Now())
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err = %v", err)
	}
}

func TestBearerAuth(t *testing.T) {
	var got string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		serve(200, `{"status":"success","data":{"resultType":"vector","result":[]}}`)(w, r)
	}, func(o *Options) { o.TokenFile = writeFile(t, "s3cret\n") })
	if _, err := c.Instant(context.Background(), "up", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer s3cret" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestBasicAuth(t *testing.T) {
	var user, pass string
	var ok bool
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok = r.BasicAuth()
		serve(200, `{"status":"success","data":{"resultType":"vector","result":[]}}`)(w, r)
	}, func(o *Options) {
		o.User = "alice"
		o.PasswordFile = writeFile(t, "pw\n")
	})
	if _, err := c.Instant(context.Background(), "up", time.Now()); err != nil {
		t.Fatal(err)
	}
	if !ok || user != "alice" || pass != "pw" {
		t.Errorf("basic auth = %q %q %v", user, pass, ok)
	}
}

func TestNoAuthHeaderByDefault(t *testing.T) {
	var got string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		serve(200, `{"status":"success","data":{"resultType":"vector","result":[]}}`)(w, r)
	}, nil)
	_, _ = c.Instant(context.Background(), "up", time.Now())
	if got != "" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestNewValidation(t *testing.T) {
	tok := writeFile(t, "t")
	empty := writeFile(t, " \n")
	tests := map[string]Options{
		"bad url":          {URL: "nope"},
		"both auth":        {URL: "http://x", TokenFile: tok, User: "u", PasswordFile: tok},
		"user no password": {URL: "http://x", User: "u"},
		"password no user": {URL: "http://x", PasswordFile: tok},
		"missing file":     {URL: "http://x", TokenFile: "/nonexistent/file"},
		"empty file":       {URL: "http://x", TokenFile: empty},
	}
	for name, o := range tests {
		if _, err := New(o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestStep(t *testing.T) {
	start := time.Unix(0, 0)
	tests := []struct {
		name  string
		span  time.Duration
		width int
		min   time.Duration
		want  time.Duration
	}{
		{"span over width", time.Hour, 120, time.Second, 30 * time.Second},
		{"min step wins", time.Minute, 120, 5 * time.Second, 5 * time.Second},
		{"zero span", 0, 120, 3 * time.Second, 3 * time.Second},
		{"rounds up to ms", time.Second, 3, time.Millisecond, 334 * time.Millisecond},
		{"zero width", time.Minute, 0, time.Second, time.Minute},
	}
	for _, tt := range tests {
		if got := Step(start, start.Add(tt.span), tt.width, tt.min); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}

	// Huge widths must still stay under the point limit.
	span := 24 * time.Hour
	step := Step(start, start.Add(span), 1_000_000, time.Millisecond)
	if n := int(span / step); n >= maxPoints {
		t.Errorf("%d points for step %v", n, step)
	}
}

func TestLabelValues(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotMatch []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		gotMatch = r.URL.Query()["match[]"]
		serve(200, `{"status":"success","data":["prod","dev","staging"]}`)(w, r)
	}, func(o *Options) { o.TokenFile = writeFile(t, "tok\n") })

	got, err := c.LabelValues(context.Background(), "namespace", `up{job="a"}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "dev,prod,staging"; strings.Join(got, ",") != want {
		t.Errorf("values = %v, want sorted %s", got, want)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/label/namespace/values" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if len(gotMatch) != 1 || gotMatch[0] != `up{job="a"}` {
		t.Errorf("match[] = %v", gotMatch)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestLabelValuesWithoutMatchAndEscapedLabel(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		serve(200, `{"status":"success","data":[]}`)(w, r)
	}, nil)
	got, err := c.LabelValues(context.Background(), "a/b", "")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	if gotPath != "/api/v1/label/a%2Fb/values" || gotQuery != "" {
		t.Errorf("path %q query %q", gotPath, gotQuery)
	}
}

func TestLabelValuesErrors(t *testing.T) {
	c := newTestClient(t, serve(400, `{"status":"error","errorType":"bad_data","error":"bad match"}`), nil)
	if _, err := c.LabelValues(context.Background(), "x", "{"); err == nil || !strings.Contains(err.Error(), "bad match") {
		t.Errorf("err = %v, want the API message", err)
	}
	c = newTestClient(t, serve(200, `{"status":"success","data":{"resultType":"matrix"}}`), nil)
	if _, err := c.LabelValues(context.Background(), "x", ""); err == nil {
		t.Error("an object where a list is expected should be an error")
	}
}
