// Package promapi is a small client for the Prometheus HTTP query API
// (/api/v1/query_range and /api/v1/query), as served by Prometheus and
// VictoriaMetrics.
package promapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxPoints is Prometheus' hard limit on points per series for a range query.
const maxPoints = 11000

// maxBody bounds how much of a response is read, so a misbehaving endpoint
// cannot exhaust memory.
const maxBody = 64 << 20

// Sample is one data point.
type Sample struct {
	T time.Time
	V float64
}

// Series is one labelled time series. Instant results and scalars have a
// single point.
type Series struct {
	Labels map[string]string
	Points []Sample
}

// Querier runs PromQL queries. The dashboard only depends on this interface.
type Querier interface {
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error)
	Instant(ctx context.Context, expr string, at time.Time) ([]Series, error)
}

// Options configures a Client. Secrets are given as file paths, never values,
// so they stay off the command line.
type Options struct {
	// URL is the base URL of the API, for example http://localhost:8428.
	URL string
	// Timeout bounds each request; zero means 30 seconds.
	Timeout time.Duration
	// TokenFile holds a bearer token. Mutually exclusive with User.
	TokenFile string
	// User and PasswordFile enable basic auth.
	User         string
	PasswordFile string
}

// Client implements Querier over HTTP.
type Client struct {
	base     string
	client   *http.Client
	token    string
	user     string
	password string
}

var _ Querier = (*Client)(nil)

// New builds a Client, reading any secret files once up front so a bad path
// fails at startup rather than on the first query.
func New(opts Options) (*Client, error) {
	u, err := url.Parse(opts.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid prometheus URL %q", opts.URL)
	}
	if opts.TokenFile != "" && opts.User != "" {
		return nil, errors.New("bearer token and basic auth are mutually exclusive")
	}
	if opts.User != "" && opts.PasswordFile == "" {
		return nil, errors.New("basic auth user requires a password file")
	}
	if opts.PasswordFile != "" && opts.User == "" {
		return nil, errors.New("password file requires a user")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	c := &Client{
		base:   strings.TrimRight(opts.URL, "/"),
		client: &http.Client{Timeout: timeout},
		user:   opts.User,
	}
	if opts.TokenFile != "" {
		if c.token, err = readSecret(opts.TokenFile); err != nil {
			return nil, err
		}
	}
	if opts.PasswordFile != "" {
		if c.password, err = readSecret(opts.PasswordFile); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading secret file: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return s, nil
}

// QueryRange runs a range query and returns one Series per matrix entry.
func (c *Client) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error) {
	form := url.Values{}
	form.Set("query", expr)
	form.Set("start", formatTime(start))
	form.Set("end", formatTime(end))
	form.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	return c.do(ctx, "/api/v1/query_range", form)
}

// Instant runs an instant query evaluated at the given time.
func (c *Client) Instant(ctx context.Context, expr string, at time.Time) ([]Series, error) {
	form := url.Values{}
	form.Set("query", expr)
	form.Set("time", formatTime(at))
	return c.do(ctx, "/api/v1/query", form)
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64)
}

// response is the common envelope of every API reply.
type response struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

func (c *Client) do(ctx context.Context, path string, form url.Values) ([]Series, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.user != "":
		req.SetBasicAuth(c.user, c.password)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}

	var r response
	jsonErr := json.Unmarshal(body, &r)

	// Prometheus reports bad queries as 4xx/5xx with a JSON error body, so
	// prefer that message over the bare status when it is present.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if jsonErr == nil && r.Status == "error" {
			return nil, apiError(&r, resp.Status)
		}
		return nil, fmt.Errorf("unexpected status %s: %s", resp.Status, snippet(body))
	}
	if jsonErr != nil {
		return nil, fmt.Errorf("malformed response: %w", jsonErr)
	}
	if r.Status != "success" {
		return nil, apiError(&r, resp.Status)
	}
	return parseResult(r.Data.ResultType, r.Data.Result)
}

func apiError(r *response, status string) error {
	msg := r.Error
	if msg == "" {
		msg = status
	}
	if r.ErrorType != "" {
		return fmt.Errorf("%s: %s", r.ErrorType, msg)
	}
	return errors.New(msg)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

type rawSeries struct {
	Metric map[string]string `json:"metric"`
	Value  *rawSample        `json:"value"`
	Values []rawSample       `json:"values"`
}

// rawSample decodes the wire form [unixSeconds, "value"]. The value is a
// string so that NaN and +Inf/-Inf survive JSON.
type rawSample struct {
	Sample
}

func (s *rawSample) UnmarshalJSON(b []byte) error {
	var pair [2]json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil {
		return fmt.Errorf("sample: %w", err)
	}
	var ts float64
	if err := json.Unmarshal(pair[0], &ts); err != nil {
		return fmt.Errorf("sample timestamp: %w", err)
	}
	var str string
	if err := json.Unmarshal(pair[1], &str); err != nil {
		return fmt.Errorf("sample value: %w", err)
	}
	// ParseFloat accepts "NaN", "+Inf" and "-Inf".
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return fmt.Errorf("sample value %q: %w", str, err)
	}
	s.T = time.UnixMilli(int64(ts*1000 + 0.5)).UTC()
	s.V = v
	return nil
}

func parseResult(typ string, raw json.RawMessage) ([]Series, error) {
	switch typ {
	case "matrix":
		var rs []rawSeries
		if err := json.Unmarshal(raw, &rs); err != nil {
			return nil, fmt.Errorf("malformed matrix: %w", err)
		}
		out := make([]Series, 0, len(rs))
		for _, s := range rs {
			pts := make([]Sample, len(s.Values))
			for i, v := range s.Values {
				pts[i] = v.Sample
			}
			out = append(out, Series{Labels: labels(s.Metric), Points: pts})
		}
		return out, nil
	case "vector":
		var rs []rawSeries
		if err := json.Unmarshal(raw, &rs); err != nil {
			return nil, fmt.Errorf("malformed vector: %w", err)
		}
		out := make([]Series, 0, len(rs))
		for _, s := range rs {
			if s.Value == nil {
				return nil, errors.New("malformed vector: missing value")
			}
			out = append(out, Series{Labels: labels(s.Metric), Points: []Sample{s.Value.Sample}})
		}
		return out, nil
	case "scalar":
		var s rawSample
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("malformed scalar: %w", err)
		}
		return []Series{{Labels: map[string]string{}, Points: []Sample{s.Sample}}}, nil
	default:
		return nil, fmt.Errorf("unsupported result type %q", typ)
	}
}

func labels(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// Step picks a query step for the window [start, end] drawn at width columns:
// max(minStep, (end-start)/width). The result is rounded up to a whole
// millisecond and widened if needed so a range query stays well under
// Prometheus' 11,000 point limit.
func Step(start, end time.Time, width int, minStep time.Duration) time.Duration {
	if minStep <= 0 {
		minStep = time.Second
	}
	span := end.Sub(start)
	if span <= 0 {
		return minStep
	}
	if width < 1 {
		width = 1
	}
	// Leave headroom below the hard limit.
	if width > maxPoints*9/10 {
		width = maxPoints * 9 / 10
	}
	step := span / time.Duration(width)
	if step%time.Millisecond != 0 {
		step += time.Millisecond - step%time.Millisecond
	}
	return max(step, minStep)
}
