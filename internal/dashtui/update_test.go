package dashtui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/michaelvl/openmetrics-tui/internal/managed"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

func TestInitQueriesEveryPanelOnce(t *testing.T) {
	q := &fakeQuerier{handle: demoHandler}
	m := newTestModel(t, mustDash(t, testDash), q, nil)
	start(m)
	if n := q.queryCount(); n != 4 {
		t.Fatalf("queries = %d, want one per panel (4): %v", n, q.queries)
	}
	if !strings.Contains(q.queries[0], `namespace="prod"`) || !strings.Contains(q.queries[0], `controller=~".*"`) {
		t.Errorf("variables not substituted: %s", q.queries[0])
	}
	// Panels are not queried again while nothing asks for it.
	if n := q.queryCount(); n != 4 {
		t.Errorf("queries = %d", n)
	}
	// A tick queries every panel again, and schedules the next tick.
	_, cmd := m.Update(tickMsg{gen: m.tickGen})
	settle(m, cmd)
	if n := q.queryCount(); n != 8 {
		t.Errorf("after a tick queries = %d, want 8", n)
	}
}

func TestTickFromOldChainIsDropped(t *testing.T) {
	q := &fakeQuerier{handle: demoHandler}
	m := newTestModel(t, mustDash(t, testDash), q, nil)
	start(m)
	old := m.tickGen
	press(m, "-")
	if m.interval != 15*time.Second || m.tickGen == old {
		t.Fatalf("interval %v tickGen %d -> %d after -", m.interval, old, m.tickGen)
	}
	_, cmd := m.Update(tickMsg{gen: old})
	settle(m, cmd)
	if q.queryCount() != 4 {
		t.Error("a tick from before the interval changed must not query")
	}
	press(m, "+", "+")
	if m.interval != 5*time.Second {
		t.Errorf("interval = %v, want 5s", m.interval)
	}
}

func TestPauseSkipsTicksButRefreshWorks(t *testing.T) {
	q := &fakeQuerier{handle: demoHandler}
	m := newTestModel(t, mustDash(t, testDash), q, nil)
	start(m)
	press(m, "p")
	if !strings.Contains(m.renderFooter(""), "PAUSED (p)") {
		t.Errorf("footer: %q", m.renderFooter(""))
	}
	_, cmd := m.Update(tickMsg{gen: m.tickGen})
	settle(m, cmd)
	if q.queryCount() != 4 {
		t.Error("paused: a tick must not query")
	}
	press(m, "r")
	if q.queryCount() != 8 {
		t.Errorf("r while paused: queries = %d, want 8", q.queryCount())
	}
}

func TestOneFailingPanelDoesNotBlankTheRest(t *testing.T) {
	q := &fakeQuerier{handle: func(ctx context.Context, query string, s, e time.Time, st time.Duration) ([]promapi.Series, error) {
		if strings.HasPrefix(query, "depth") {
			return nil, errBoom
		}
		return demoHandler(ctx, query, s, e, st)
	}}
	m := newTestModel(t, mustDash(t, testDash), q, nil)
	start(m)
	for i, p := range m.panels {
		if (p.err != nil) != (i == 2) {
			t.Errorf("panel %d err = %v", i, p.err)
		}
	}
	if len(m.panels[0].rows) != 3 {
		t.Error("healthy panel lost its rows")
	}
	// A later success clears the error.
	q.handle = demoHandler
	press(m, "r")
	if m.panels[2].err != nil || len(m.panels[2].rows) != 2 {
		t.Errorf("recovery: err %v rows %d", m.panels[2].err, len(m.panels[2].rows))
	}
}

func TestBackoffOnRepeatedErrors(t *testing.T) {
	clock := testNow
	q := &fakeQuerier{handle: func(context.Context, string, time.Time, time.Time, time.Duration) ([]promapi.Series, error) {
		return nil, errBoom
	}}
	m := newTestModel(t, mustDash(t, testDash), q, func(o *Options) { o.Now = func() time.Time { return clock } })
	start(m)
	if q.queryCount() != 4 {
		t.Fatalf("queries = %d", q.queryCount())
	}
	// First failure: wait two intervals (20s).
	if got := m.panels[0].retryAt.Sub(clock); got != 20*time.Second {
		t.Errorf("retry after %v, want 20s", got)
	}
	tick := func() {
		_, cmd := m.Update(tickMsg{gen: m.tickGen})
		settle(m, cmd)
	}
	clock = clock.Add(10 * time.Second)
	tick()
	if q.queryCount() != 4 {
		t.Error("panel in back-off was queried on the next tick")
	}
	clock = clock.Add(10 * time.Second)
	tick()
	if q.queryCount() != 8 {
		t.Errorf("queries = %d, want a retry once the back-off passed", q.queryCount())
	}
	// Keeps doubling, up to the cap.
	if got := m.panels[0].retryAt.Sub(clock); got != 40*time.Second {
		t.Errorf("second back-off %v, want 40s", got)
	}
	for range 10 {
		clock = clock.Add(maxBackoff)
		tick()
	}
	if got := m.panels[0].retryAt.Sub(clock); got != maxBackoff {
		t.Errorf("back-off %v, want it capped at %v", got, maxBackoff)
	}
	// r ignores the back-off.
	n := q.queryCount()
	press(m, "r")
	if q.queryCount() != n+4 {
		t.Error("r should retry panels in back-off")
	}
}

// Applying a variable cancels requests still out and drops their results.
func TestVariableChangeCancelsInflightAndDropsStaleResults(t *testing.T) {
	release := make(chan struct{})
	cancelled := make(chan string, 8)
	q := &fakeQuerier{handle: func(ctx context.Context, query string, s, e time.Time, st time.Duration) ([]promapi.Series, error) {
		if strings.Contains(query, `namespace="prod"`) {
			select {
			case <-ctx.Done():
				cancelled <- query
				return nil, ctx.Err()
			case <-release:
			}
		}
		return demoHandler(ctx, query, s, e, st)
	}}
	d := mustDash(t, testDash)
	m := newTestModel(t, d, q, nil)

	cmds := m.dispatchAll(true)
	if len(cmds) != 4 {
		t.Fatalf("dispatched %d", len(cmds))
	}
	// Run the old-generation queries in the background; they block on ctx.
	stale := make(chan tea.Msg, 4)
	for _, c := range cmds {
		go func() { stale <- c() }()
	}
	oldGen, oldTick := m.gen, m.tickGen

	press(m, "1")
	m.input.SetValue("staging")
	press(m, "enter")
	if m.vals["namespace"] != "staging" {
		t.Fatalf("namespace = %q", m.vals["namespace"])
	}
	if m.gen == oldGen || m.tickGen == oldTick {
		t.Error("applying a variable must start a new generation and tick chain")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the in-flight request was not cancelled")
	}
	// Whatever the old generation returns is ignored.
	for range 4 {
		select {
		case msg := <-stale:
			m.Update(msg)
		case <-time.After(2 * time.Second):
			t.Fatal("stale commands did not return")
		}
	}
	for i, p := range m.panels {
		if p.err != nil && errors.Is(p.err, context.Canceled) {
			t.Errorf("panel %d shows a cancelled request as an error", i)
		}
	}
	// The new generation queried with the new value (the fake returns at once
	// for anything but prod).
	q.mu.Lock()
	var staging int
	for _, s := range q.queries {
		if strings.Contains(s, `namespace="staging"`) {
			staging++
		}
	}
	q.mu.Unlock()
	if staging != 3 { // the apiserver panel has no namespace
		t.Errorf("queries for staging = %d, want 3", staging)
	}
	close(release)
}

func TestVariableEditing(t *testing.T) {
	q := &fakeQuerier{handle: demoHandler}
	m := newTestModel(t, mustDash(t, testDash), q, nil)
	start(m)

	press(m, "1")
	if m.edit.kind != editVar || m.input.Value() != "prod" {
		t.Fatalf("edit = %+v value %q", m.edit, m.input.Value())
	}
	// While a box is open every key goes to the input, digits and q included.
	typeText(m, "2q")
	if m.input.Value() != "prod2q" || m.edit.kind != editVar {
		t.Errorf("value %q, edit %v", m.input.Value(), m.edit.kind)
	}
	// A value failing the pattern keeps the box open and says why.
	typeText(m, "!")
	press(m, "enter")
	if m.edit.kind != editVar || !strings.Contains(m.renderFooter(""), "invalid: $namespace") {
		t.Errorf("footer after refused apply: %q", m.renderFooter(""))
	}
	if q.queryCount() != 4 {
		t.Error("an invalid value must not query")
	}
	// Typing clears the complaint; esc cancels without applying.
	press(m, "backspace")
	if m.edit.err != "" {
		t.Error("typing should clear the validation error")
	}
	press(m, "esc")
	if m.edit.kind != editNone || m.vals["namespace"] != "prod" {
		t.Errorf("esc: edit %v value %q", m.edit.kind, m.vals["namespace"])
	}

	// Applying the unchanged value is not a refresh.
	press(m, "1", "enter")
	if q.queryCount() != 4 {
		t.Errorf("unchanged value queried: %d", q.queryCount())
	}

	// Tab applies the box and moves to the next; regex values are allowed in
	// $controller.
	press(m, "1")
	for range 4 {
		press(m, "backspace")
	}
	typeText(m, "dev")
	press(m, "tab")
	if m.edit.kind != editVar || m.edit.v != 1 || m.vals["namespace"] != "dev" {
		t.Fatalf("tab: edit %+v vals %v", m.edit, m.vals)
	}
	for range 2 {
		press(m, "backspace")
	}
	typeText(m, "gad.*")
	press(m, "enter")
	if m.vals["controller"] != "gad.*" || q.queryCount() != 12 {
		t.Errorf("controller %q queries %d", m.vals["controller"], q.queryCount())
	}
	last := q.queries[len(q.queries)-4]
	if !strings.Contains(last, `namespace="dev"`) || !strings.Contains(last, `controller=~"gad.*"`) {
		t.Errorf("query %s", last)
	}
	// An invalid regex in $controller is refused.
	press(m, "2", "(")
	press(m, "enter")
	if m.edit.kind != editVar || !strings.Contains(m.edit.err, "not a valid regex") {
		t.Errorf("edit %+v", m.edit)
	}
	press(m, "esc")
}

func TestOnlyDeclaredVariableKeysOpenABox(t *testing.T) {
	m := newTestModel(t, mustDash(t, testDash), &fakeQuerier{}, nil)
	press(m, "3")
	if m.edit.kind != editNone {
		t.Error("key 3 opened a box with only two variables")
	}
}

func TestKeysMoveActivePanelAndFold(t *testing.T) {
	m := demoModel(t, 100, 40)
	press(m, "tab")
	if m.active != 1 {
		t.Errorf("active = %d", m.active)
	}
	press(m, "shift+tab", "shift+tab")
	if m.active != 3 {
		t.Errorf("shift+tab wraps: active = %d", m.active)
	}
	press(m, "tab")
	if m.active != 0 {
		t.Errorf("tab wraps: active = %d", m.active)
	}
	press(m, "down", "down", "down", "down")
	if m.panels[0].cursor != 2 {
		t.Errorf("cursor = %d, want clamped to 2", m.panels[0].cursor)
	}
	press(m, "up")
	if m.panels[0].cursor != 1 {
		t.Errorf("cursor = %d", m.panels[0].cursor)
	}
	press(m, " ")
	if !m.panels[0].folded {
		t.Error("space should fold the active panel")
	}
	press(m, "down") // on a folded panel the arrows change panel
	if m.active != 1 {
		t.Errorf("down on a folded panel: active = %d", m.active)
	}
	press(m, "shift+tab", "enter")
	if m.panels[0].folded {
		t.Error("enter should unfold")
	}
}

func TestSnapshotWritesPlainWholeDashboard(t *testing.T) {
	dir := t.TempDir()
	m := newTestModel(t, mustDash(t, testDash), &fakeQuerier{handle: demoHandler}, func(o *Options) { o.SnapshotDir = dir })
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 10}) // too short for everything
	start(m)
	m.Update(key("S"))
	files, _ := filepath.Glob(filepath.Join(dir, "omtui-dash-snapshot-*.txt"))
	if len(files) != 1 {
		t.Fatalf("snapshot files = %v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"# omtui-dash snapshot", "$namespace (1)", "API p99 latency", "(single series)", "database-controller"} {
		if !strings.Contains(s, want) {
			t.Errorf("snapshot lacks %q", want)
		}
	}
	if strings.Contains(s, "\x1b") {
		t.Error("snapshot contains escape codes")
	}
	if f := m.renderFooter(""); !strings.Contains(f, "saved ") {
		t.Errorf("footer lacks the saved message: %q", f)
	}
	// The message clears, but only for its own save.
	m.Update(clearSnapshotMsg{gen: m.snapshotGen - 1})
	if m.snapshotMsg == "" {
		t.Error("an older timer cleared a newer message")
	}
	m.Update(clearSnapshotMsg{gen: m.snapshotGen})
	if m.snapshotMsg != "" {
		t.Error("message not cleared")
	}
	// A write failure is reported in the footer.
	m.snapDir = filepath.Join(dir, "missing")
	m.Update(key("S"))
	if !m.snapshotErr || !strings.Contains(m.snapshotMsg, "snapshot failed") {
		t.Errorf("snapshot msg %q err=%v", m.snapshotMsg, m.snapshotErr)
	}
}

func TestHelpOverlay(t *testing.T) {
	m := demoModel(t, 100, 40)
	press(m, "?")
	if v := m.View(); !strings.Contains(v, "Help") || !strings.Contains(v, "Fold or unfold") {
		t.Errorf("help not shown:\n%s", v)
	}
	press(m, "?")
	if strings.Contains(m.View(), "Help") {
		t.Error("? should close the help")
	}
}

func TestQuitCancelsContext(t *testing.T) {
	m := newTestModel(t, mustDash(t, testDash), &fakeQuerier{}, nil)
	ctx := m.ctx
	_, cmd := m.Update(key("q"))
	if cmd == nil || ctx.Err() == nil {
		t.Error("q should quit and cancel in-flight requests")
	}
}

// --- suggestions ---

const varDash = `title: T
variables:
  namespace:
    default: prod
    pattern: '^[a-z0-9-]+$'
  controller:
    default: ".*"
    regex: true
  fixed:
    default: a
    options: [a, b, c]
  plain:
    default: x
    options: false
panels:
  - title: P
    query: up{namespace="${namespace}", controller=~"${controller}", fixed="${fixed}", plain="${plain}"}
`

func varModel(t *testing.T, q *fakeQuerier) *Model {
	t.Helper()
	m := newTestModel(t, mustDash(t, varDash), q, nil)
	start(m)
	return m
}

func nsValues(label, match string) ([]string, error) {
	switch label {
	case "namespace":
		return []string{"staging", "prod", "dev", "kube-system", "monitoring", "a1", "a2", "a3", "a4", "a5"}, nil
	case "controller":
		return []string{"widget", "gadget"}, nil
	}
	return nil, nil
}

func TestSuggestionsFetchedOnOpenFilteredAndAccepted(t *testing.T) {
	q := &fakeQuerier{labelValues: nsValues}
	m := varModel(t, q)
	press(m, "1")
	if q.labelCount() != 1 {
		t.Fatalf("lookups = %d, want 1 when the box opens", q.labelCount())
	}
	// The matcher for the variable being edited is dropped; the others are
	// substituted with their current values (cascading).
	call := q.labelCalls[0]
	if !strings.HasPrefix(call, "namespace|up{") ||
		strings.Contains(call, "namespace=") ||
		!strings.Contains(call, `controller=~".*"`) || !strings.Contains(call, `fixed="a"`) {
		t.Errorf("lookup = %s", call)
	}
	if m.edit.sug != sugReady || len(m.edit.all) != 10 {
		t.Fatalf("suggestions: %+v", m.edit)
	}
	// 10 values, 8 shown, "2 more" and the count in the footer.
	v := m.View()
	if !strings.Contains(v, "… 2 more") || !strings.Contains(v, "▸ prod") {
		t.Errorf("overlay missing:\n%s", v)
	}
	if got := strings.Count(v, "│ "); got < 8 {
		t.Errorf("want at least 8 rows in the overlay, saw %d", got)
	}
	if f := m.renderFooter(""); !strings.Contains(f, "10 of 10 values") {
		t.Errorf("footer: %q", f)
	}
	// Substring filter, case insensitive; the typed text is not highlighted.
	for range 4 {
		press(m, "backspace")
	}
	typeText(m, "ING")
	if got := m.filtered(); len(got) != 2 || got[0] != "monitoring" && got[1] != "staging" {
		t.Errorf("filtered = %v", got)
	}
	if f := m.renderFooter(""); !strings.Contains(f, "2 of 10 values") || !strings.Contains(f, "not in list") {
		t.Errorf("footer: %q", f)
	}
	// Down selects, enter accepts the highlighted value and applies it.
	press(m, "down", "down")
	if m.edit.hl != 1 {
		t.Fatalf("hl = %d", m.edit.hl)
	}
	press(m, "enter")
	if m.vals["namespace"] != "staging" || m.edit.kind != editNone {
		t.Errorf("namespace = %q edit %v", m.vals["namespace"], m.edit.kind)
	}
}

func TestTabAcceptsHighlightThenMoves(t *testing.T) {
	m := varModel(t, &fakeQuerier{labelValues: nsValues})
	press(m, "1")
	press(m, "down") // from the current value (prod) to the next
	hl := m.filtered()[m.edit.hl]
	press(m, "tab")
	if m.input.Value() != hl || m.edit.v != 0 || m.vals["namespace"] != "prod" {
		t.Errorf("tab with a highlight should only fill the box: value %q v %d vals %v", m.input.Value(), m.edit.v, m.vals)
	}
	press(m, "tab") // no highlight now: apply and move on
	if m.edit.v != 1 || m.vals["namespace"] != hl {
		t.Errorf("second tab: v %d namespace %q", m.edit.v, m.vals["namespace"])
	}
}

func TestSuggestionsAreCachedBriefly(t *testing.T) {
	clock := testNow
	q := &fakeQuerier{labelValues: nsValues}
	m := newTestModel(t, mustDash(t, varDash), q, func(o *Options) { o.Now = func() time.Time { return clock } })
	start(m)
	press(m, "1", "esc", "1")
	if q.labelCount() != 1 || m.edit.sug != sugReady {
		t.Errorf("lookups = %d state %v, want the second open served from the cache", q.labelCount(), m.edit.sug)
	}
	press(m, "esc")
	clock = clock.Add(lookupTTL + time.Second)
	press(m, "1")
	if q.labelCount() != 2 {
		t.Errorf("lookups = %d, want a refetch after the TTL", q.labelCount())
	}
}

func TestSuggestionsFallBackToFreeText(t *testing.T) {
	for name, lv := range map[string]func(string, string) ([]string, error){
		"failed": func(string, string) ([]string, error) { return nil, errBoom },
		"empty":  func(string, string) ([]string, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			m := varModel(t, &fakeQuerier{labelValues: lv})
			press(m, "1")
			if m.edit.sug != sugFailed || len(m.filtered()) != 0 {
				t.Fatalf("state %v", m.edit.sug)
			}
			if f := m.renderFooter(""); !strings.Contains(f, "free text") {
				t.Errorf("footer: %q", f)
			}
			typeText(m, "x")
			press(m, "enter") // free text still applies
			if m.vals["namespace"] != "prodx" {
				t.Errorf("namespace = %q", m.vals["namespace"])
			}
		})
	}
}

func TestNoLabelValuerIsFreeText(t *testing.T) {
	// A Querier without LabelValues: plain wrapper.
	m := newTestModel(t, mustDash(t, varDash), onlyQuerier{&fakeQuerier{}}, nil)
	press(m, "1")
	if m.edit.sug != sugFailed {
		t.Errorf("state %v", m.edit.sug)
	}
}

type onlyQuerier struct{ q promapi.Querier }

func (o onlyQuerier) QueryRange(ctx context.Context, e string, s, en time.Time, st time.Duration) ([]promapi.Series, error) {
	return o.q.QueryRange(ctx, e, s, en, st)
}

func (o onlyQuerier) Instant(ctx context.Context, e string, at time.Time) ([]promapi.Series, error) {
	return o.q.Instant(ctx, e, at)
}

func TestExplicitOptions(t *testing.T) {
	q := &fakeQuerier{labelValues: nsValues}
	m := varModel(t, q)
	press(m, "3") // options: [a, b, c], no lookup
	if m.edit.sug != sugReady || strings.Join(m.edit.all, ",") != "a,b,c" || q.labelCount() != 0 {
		t.Errorf("list %v lookups %d", m.edit.all, q.labelCount())
	}
	press(m, "esc", "4") // options: false
	if m.edit.sug != sugNone || q.labelCount() != 0 {
		t.Errorf("state %v lookups %d", m.edit.sug, q.labelCount())
	}
	press(m, "esc")

	d := mustDash(t, strings.Replace(varDash, "    options: [a, b, c]", "    options: {label_values: {metric: kube_pod_info, label: fixed_label}}", 1))
	m = newTestModel(t, d, q, nil)
	start(m)
	press(m, "3")
	if q.labelCalls[len(q.labelCalls)-1] != "fixed_label|kube_pod_info" {
		t.Errorf("lookups %v", q.labelCalls)
	}
}

func TestOverlayShowsEightRowsWithHighlightScrolled(t *testing.T) {
	m := varModel(t, &fakeQuerier{labelValues: nsValues})
	press(m, "1")
	for range 10 {
		press(m, "down")
	}
	if m.edit.hl != 9 {
		t.Fatalf("hl = %d", m.edit.hl)
	}
	v := m.View()
	if !strings.Contains(v, "▸ staging") {
		t.Errorf("highlight not in view:\n%s", v)
	}
	if strings.Count(v, "\n") != m.height-1 {
		t.Error("overlay changed the screen height")
	}
}

func TestOverlayKeepsHeaderHeight(t *testing.T) {
	m := varModel(t, &fakeQuerier{labelValues: nsValues})
	before := strings.Split(m.View(), "\n")
	press(m, "1")
	after := strings.Split(m.View(), "\n")
	if len(before) != len(after) {
		t.Errorf("screen height %d -> %d", len(before), len(after))
	}
	golden(t, "overlay_100", m.View())
	// The overlay sits in the panel area, under the box, not in the header.
	if !strings.HasPrefix(after[4], "╭") {
		t.Errorf("row 4 should start the overlay: %q", after[4])
	}
}

// --- managed ---

func managedModel(t *testing.T) (*Model, *fakeBackend) {
	t.Helper()
	b := newFakeBackend()
	m := newTestModel(t, mustDash(t, testDash), &fakeQuerier{handle: demoHandler}, func(o *Options) {
		o.Backend, o.Mode = b, "managed ctx=dev"
	})
	start(m)
	return m, b
}

func TestManagedStatusAndHealth(t *testing.T) {
	m, b := managedModel(t)
	if v := m.View(); !strings.HasPrefix(v, m.d.Title) || !strings.Contains(v, "● ok") {
		t.Errorf("title or footer status missing:\n%s", v)
	}
	if f := m.renderFooter(""); !strings.Contains(f, "Log (L)") || strings.Contains(f, "Restart (R)") {
		t.Errorf("footer: %q", f)
	}
	// A child dies: the event refreshes health, the banner appears.
	b.kill("victoria-metrics", errors.New("exit status 2"))
	m.Update(eventMsg{ev: managed.Event{Child: "victoria-metrics"}})
	settle(m, m.healthCmd())
	v := m.View()
	for _, want := range []string{"⚠ backend down", "⚠ victoria-metrics exited: exit status 2", "R restart, L logs", "vm: scraping 3 targets"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if f := m.renderFooter(""); !strings.Contains(f, "Restart (R)") {
		t.Errorf("footer: %q", f)
	}
	checkShape(t, m)
	golden(t, "managed_down_100", v)
}

func TestRestartKey(t *testing.T) {
	m, b := managedModel(t)
	b.kill("victoria-metrics", errors.New("boom"))
	settle(m, m.healthCmd())
	n := m.panels[0].inflight
	_ = n
	_, cmd := m.Update(key("R"))
	if !m.restarting || !strings.Contains(m.View(), "restarting") {
		t.Errorf("not restarting:\n%s", m.View())
	}
	// A second R while restarting does nothing.
	if _, again := m.Update(key("R")); again != nil {
		t.Error("R during a restart should be ignored")
	}
	before := m.panels[0].rows
	_ = before
	settle(m, cmd)
	if b.restarts != 1 || m.restarting {
		t.Errorf("restarts %d restarting %v", b.restarts, m.restarting)
	}
	settle(m, m.healthCmd())
	if strings.Contains(m.View(), "down") {
		t.Errorf("still down after restart:\n%s", m.View())
	}

	// A failed restart is reported.
	b.restartE = errors.New("kubectl proxy: port busy")
	press(m, "R")
	if !strings.Contains(m.View(), "restart failed: kubectl proxy: port busy") {
		t.Errorf("view:\n%s", m.View())
	}
}

func TestLogOverlay(t *testing.T) {
	m, b := managedModel(t)
	for i := range 30 {
		b.logs = append(b.logs, "line "+string(rune('a'+i%26)))
	}
	press(m, "L")
	v := m.View()
	if !m.logOpen || !strings.Contains(v, "Backend log, 32 lines") || !strings.Contains(v, "line d") {
		t.Errorf("log overlay:\n%s", v)
	}
	golden(t, "log_100", v)
	// Keys go to the overlay, not the panels.
	press(m, "k", "k")
	if m.logScroll != 2 {
		t.Errorf("logScroll = %d", m.logScroll)
	}
	press(m, "z")
	if m.panels[0].folded {
		t.Error("z acted on the panels while the log was open")
	}
	press(m, "esc")
	if m.logOpen {
		t.Error("esc should close the log")
	}
	checkShape(t, m)
}

func TestManagedKeysAreInertForExternalBackends(t *testing.T) {
	m := demoModel(t, 100, 40)
	press(m, "L", "R")
	if m.logOpen || m.restarting {
		t.Error("L and R only apply to a managed backend")
	}
	if f := m.renderFooter(""); strings.Contains(f, "Log (L)") {
		t.Errorf("footer advertises L: %q", f)
	}
}

func TestStepInterval(t *testing.T) {
	cases := []struct {
		cur    time.Duration
		faster bool
		want   time.Duration
	}{
		{10 * time.Second, true, 5 * time.Second},
		{10 * time.Second, false, 15 * time.Second},
		{7 * time.Second, true, 5 * time.Second},
		{7 * time.Second, false, 10 * time.Second},
		{time.Second, true, time.Second},
		{5 * time.Minute, false, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := stepInterval(c.cur, c.faster); got != c.want {
			t.Errorf("stepInterval(%v, %v) = %v, want %v", c.cur, c.faster, got, c.want)
		}
	}
}
