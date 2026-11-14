package dashtui

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/managed"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

var update = flag.Bool("update", false, "rewrite golden files")

var errBoom = errors.New("bad_data: boom")

// TestMain turns colour off so the golden strings are plain text.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

var testNow = time.Date(2024, 3, 1, 14, 2, 11, 0, time.UTC)

const testDash = `title: Controller health
refresh: 10s
windows: [now, 5m, 15m, 1h]
spark: 1h
variables:
  namespace:
    default: prod
    pattern: '^[a-z0-9-]+$'
  controller:
    default: ".*"
    regex: true
panels:
  - title: Reconciles per minute
    query: sum by (controller) (rate(reconcile_total{namespace="${namespace}", controller=~"${controller}"}[5m])) * 60
    unit: "/min"
    label: "{{controller}}"
    thresholds:
      - { gt: 5, color: red }
      - { gt: 1, color: yellow }
      - { lte: 1, color: green }
  - title: Error ratio
    query: errors{namespace="${namespace}", controller=~"${controller}"}
    unit: percent
    label: "{{controller}}"
    thresholds:
      - { gt: 0.05, color: red }
      - { gt: 0.01, color: yellow }
      - { color: green }
  - title: Workqueue depth
    query: depth{namespace="${namespace}"}
    label: "{{name}}"
    sort: desc
    limit: 8
    thresholds:
      - { gt: 100, color: red }
      - { color: green }
  - title: API p99 latency
    query: apiserver_p99
    unit: s
    thresholds:
      - { gt: 1, color: red }
      - { color: green }
`

func mustDash(t *testing.T, src string) *dashboard.Dashboard {
	t.Helper()
	d, err := dashboard.Parse([]byte(src))
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	return d
}

// lin is a series with points at the step from start to end, rising linearly
// from v0 at the start to v1 at the end.
func lin(labels map[string]string, start, end time.Time, step time.Duration, v0, v1 float64) promapi.Series {
	s := promapi.Series{Labels: labels}
	n := int(end.Sub(start) / step)
	for i := 0; i <= n; i++ {
		f := float64(i) / float64(max(n, 1))
		s.Points = append(s.Points, promapi.Sample{T: start.Add(time.Duration(i) * step), V: v0 + (v1-v0)*f})
	}
	return s
}

func lbl(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

type handlerFunc func(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]promapi.Series, error)

// fakeQuerier answers through handle and records what it was asked.
type fakeQuerier struct {
	mu      sync.Mutex
	queries []string
	handle  handlerFunc

	labelCalls  []string
	labelValues func(label, match string) ([]string, error)
}

func (f *fakeQuerier) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]promapi.Series, error) {
	f.mu.Lock()
	f.queries = append(f.queries, expr)
	h := f.handle
	f.mu.Unlock()
	if h == nil {
		return nil, nil
	}
	return h(ctx, expr, start, end, step)
}

func (f *fakeQuerier) Instant(context.Context, string, time.Time) ([]promapi.Series, error) {
	return nil, nil
}

func (f *fakeQuerier) LabelValues(_ context.Context, label, match string) ([]string, error) {
	f.mu.Lock()
	f.labelCalls = append(f.labelCalls, label+"|"+match)
	lv := f.labelValues
	f.mu.Unlock()
	if lv == nil {
		return nil, nil
	}
	return lv(label, match)
}

func (f *fakeQuerier) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeQuerier) labelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.labelCalls)
}

// demoHandler serves the standard fixture, one series set per panel.
func demoHandler(_ context.Context, q string, start, end time.Time, step time.Duration) ([]promapi.Series, error) {
	switch {
	case strings.HasPrefix(q, "sum by (controller)"):
		return []promapi.Series{
			lin(lbl("controller", "widget-controller"), start, end, step, 0.6, 0.4),
			lin(lbl("controller", "gadget-controller"), start, end, step, 0.9, 3.2),
			lin(lbl("controller", "database-controller"), start, end, step, 6.2, 7.9),
		}, nil
	case strings.HasPrefix(q, "errors"):
		return []promapi.Series{
			lin(lbl("controller", "widget-controller"), start, end, step, 0, 0),
			lin(lbl("controller", "gadget-controller"), start, end, step, 0.001, 0.014),
			lin(lbl("controller", "database-controller"), start, end, step, 0.085, 0.091),
		}, nil
	case strings.HasPrefix(q, "depth"):
		return []promapi.Series{
			lin(lbl("name", "widget"), start, end, step, 0, 2),
			lin(lbl("name", "gadget"), start, end, step, 3, 143),
		}, nil
	case strings.HasPrefix(q, "apiserver"):
		return []promapi.Series{lin(lbl(), start, end, step, 0.22, 0.21)}, nil
	}
	return nil, nil
}

// newTestModel builds a model with a fixed clock and no timers, at 100x40.
func newTestModel(t *testing.T, d *dashboard.Dashboard, q promapi.Querier, mod func(*Options)) *Model {
	t.Helper()
	opts := Options{
		Dashboard: d, Values: map[string]string{}, Querier: q, Mode: "external http://prom:9090",
		Ctx: t.Context(), Now: func() time.Time { return testNow }, SnapshotDir: t.TempDir(),
	}
	if mod != nil {
		mod(&opts)
	}
	m := New(opts)
	m.tickFn = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return m
}

// settle runs cmd and feeds every resulting message back into the model,
// recursively. Commands that block (event waits) are abandoned after a moment.
func settle(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(150 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			settle(m, c)
		}
	default:
		_, next := m.Update(msg)
		settle(m, next)
	}
}

// start runs Init and lets every first query come back.
func start(m *Model) { settle(m, m.Init()) }

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press sends keys one by one, settling the commands each returns.
func press(m *Model, keys ...string) {
	for _, k := range keys {
		_, cmd := m.Update(key(k))
		settle(m, cmd)
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		press(m, string(r))
	}
}

// golden compares got with testdata/<name>.golden, rewriting it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	var lines []string
	for _, l := range strings.Split(got, "\n") {
		lines = append(lines, strings.TrimRight(l, " ")) // padding, not content
	}
	got = strings.Join(lines, "\n") + "\n"
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// fakeBackend is a managed backend whose children can be failed on demand.
type fakeBackend struct {
	mu       sync.Mutex
	health   []managed.ChildStatus
	logs     []string
	events   chan managed.Event
	restarts int
	restartE error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		events: make(chan managed.Event, 1),
		health: []managed.ChildStatus{{Name: "kubectl proxy", Running: true}, {Name: "victoria-metrics", Running: true}},
		logs:   []string{"vm: started", "vm: scraping 3 targets"},
	}
}

func (b *fakeBackend) Health() []managed.ChildStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]managed.ChildStatus(nil), b.health...)
}

func (b *fakeBackend) Events() <-chan managed.Event { return b.events }

func (b *fakeBackend) Logs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.logs...)
}

func (b *fakeBackend) Restart(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.restarts++
	if b.restartE == nil {
		for i := range b.health {
			b.health[i].Running, b.health[i].Err = true, nil
		}
	}
	return b.restartE
}

func (b *fakeBackend) kill(name string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.health {
		if b.health[i].Name == name {
			b.health[i].Running, b.health[i].Err = false, err
		}
	}
}
