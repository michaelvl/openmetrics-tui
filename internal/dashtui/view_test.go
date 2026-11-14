package dashtui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

func demoModel(t *testing.T, width, height int) *Model {
	t.Helper()
	m := newTestModel(t, mustDash(t, testDash), &fakeQuerier{handle: demoHandler}, nil)
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	start(m)
	return m
}

// checkShape verifies the screen fills the height and no line overruns the
// width (the view is colourless in tests, so runes are cells).
func checkShape(t *testing.T, m *Model) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Errorf("view has %d lines, want %d", len(lines), m.height)
	}
	for i, l := range lines {
		if w := len([]rune(l)); w > m.width {
			t.Errorf("line %d is %d cells, width %d: %q", i, w, m.width, l)
		}
	}
}

func TestViewExpanded(t *testing.T) {
	m := demoModel(t, 100, 40)
	golden(t, "expanded_100", m.View())
	checkShape(t, m)
}

func TestViewNarrowDropsSparklineThenLongestWindow(t *testing.T) {
	m := demoModel(t, 60, 40)
	golden(t, "narrow_60", m.View())
	if strings.ContainsAny(m.View(), "▁▂▃▄▅▆▇█") {
		t.Error("sparkline should be dropped at 60 columns")
	}
	checkShape(t, m)

	m = demoModel(t, 44, 40)
	golden(t, "narrow_44", m.View())
	v := m.View()
	if strings.Contains(v, " 1h") || !strings.Contains(v, "now") || !strings.Contains(v, "5m") {
		t.Errorf("at 44 columns the 1h window should go first:\n%s", v)
	}
	checkShape(t, m)
}

func TestViewFolded(t *testing.T) {
	m := demoModel(t, 100, 40)
	press(m, "z") // everything is expanded, so z folds it all
	press(m, " ") // unfold the active (first) panel
	golden(t, "mixed_100", m.View())
	press(m, " ")
	golden(t, "folded_100", m.View())
	checkShape(t, m)
	press(m, "z") // everything folded, so z unfolds it all
	for i, p := range m.panels {
		if p.folded {
			t.Errorf("panel %d still folded after z on a fully folded dashboard", i)
		}
	}
}

func TestFoldedSummaryCountsAndWorst(t *testing.T) {
	m := demoModel(t, 100, 40)
	press(m, "z")
	v := m.View()
	for _, want := range []string{
		"▸ Reconciles per minute (/min)", "▲1/3 >5  ■1/3 >1  ●1/3 ≤1", "worst: database-controller 7.9",
		"worst: gadget 143", "● 0.21",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("folded view lacks %q:\n%s", want, v)
		}
	}
}

func TestViewFoldedFromYAML(t *testing.T) {
	d := mustDash(t, strings.Replace(testDash, "  - title: Error ratio", "  - title: Error ratio\n    folded: true", 1))
	m := newTestModel(t, d, &fakeQuerier{handle: demoHandler}, nil)
	start(m)
	if !m.panels[1].folded || m.panels[0].folded {
		t.Fatalf("initial fold state: %v %v", m.panels[0].folded, m.panels[1].folded)
	}
	// A folded panel keeps querying, so its summary is live.
	if v := m.View(); !strings.Contains(v, "▸ Error ratio") || !strings.Contains(v, "worst: database-controller 9.1%") {
		t.Errorf("folded panel should show a live summary:\n%s", v)
	}
}

func TestViewErrorAndNoData(t *testing.T) {
	q := &fakeQuerier{handle: func(ctx context.Context, q string, s, e time.Time, st time.Duration) ([]promapi.Series, error) {
		switch {
		case strings.HasPrefix(q, "errors"):
			return nil, errBoom
		case strings.HasPrefix(q, "depth"):
			return nil, nil
		}
		return demoHandler(ctx, q, s, e, st)
	}}
	m := newTestModel(t, mustDash(t, testDash), q, nil)
	start(m)
	golden(t, "error_nodata_100", m.View())
	v := m.View()
	if !strings.Contains(v, "bad_data: boom") || !strings.Contains(v, "no data (not scraped?)") {
		t.Errorf("view:\n%s", v)
	}
	if !strings.Contains(v, "database-controller") {
		t.Error("healthy panels should still show rows")
	}
	if !strings.Contains(m.renderFooter(""), "1 of 4 panels failing") {
		t.Errorf("footer: %q", m.renderFooter(""))
	}
	press(m, "z")
	v = m.View()
	if !strings.Contains(v, "bad_data: boom") || !strings.Contains(v, "no data (not scraped?)") {
		t.Errorf("folded view:\n%s", v)
	}
}

func TestViewLoadingBeforeFirstResponse(t *testing.T) {
	m := newTestModel(t, mustDash(t, testDash), &fakeQuerier{handle: demoHandler}, nil)
	if !strings.Contains(m.View(), "loading…") || !strings.Contains(m.renderFooter(""), "loading") {
		t.Errorf("before any response:\n%s", m.View())
	}
}

// A backend with less history than the longest window: older columns show a
// placeholder and the title carries a note.
func TestViewColdStart(t *testing.T) {
	q := &fakeQuerier{handle: func(_ context.Context, query string, _, end time.Time, step time.Duration) ([]promapi.Series, error) {
		if !strings.HasPrefix(query, "sum by (controller)") {
			return nil, nil
		}
		return []promapi.Series{lin(lbl("controller", "widget-controller"), end.Add(-3*time.Minute), end, step, 0.3, 0.4)}, nil
	}}
	m := newTestModel(t, mustDash(t, testDash), q, nil)
	start(m)
	golden(t, "coldstart_100", m.View())
	v := m.View()
	if !strings.Contains(v, "collecting data (3 min)") {
		t.Errorf("want the cold-start note:\n%s", v)
	}
	var row string
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, "widget-controller") {
			row = l
		}
	}
	if !strings.Contains(row, "·") || !strings.Contains(row, "0.4●") {
		t.Errorf("row %q: want the now value and placeholders", row)
	}
}

func TestViewScrollsOnShortTerminal(t *testing.T) {
	m := demoModel(t, 100, 12)
	checkShape(t, m)
	if !strings.Contains(m.View(), "▼") {
		t.Error("footer should hint at content below")
	}
	press(m, "shift+tab") // the last panel must scroll into view
	if v := m.View(); !strings.Contains(v, "API p99 latency") || !strings.Contains(v, "0.21") {
		t.Errorf("last panel not visible after shift+tab:\n%s", v)
	}
	checkShape(t, m)

	m = demoModel(t, 100, 8)
	press(m, "down", "down")
	if !strings.Contains(m.View(), "▌ database-controller") {
		t.Errorf("cursor row scrolled out of view:\n%s", m.View())
	}
	checkShape(t, m)
}

func TestRowFilterAndSort(t *testing.T) {
	m := demoModel(t, 100, 40)
	press(m, "/")
	typeText(m, "gadget")
	if v := m.View(); strings.Contains(v, "widget-controller") || !strings.Contains(v, "gadget-controller") {
		t.Errorf("filter not applied as typed:\n%s", v)
	}
	press(m, "enter")
	if f := m.renderFooter(""); !strings.Contains(f, "Filter (/): gadget") {
		t.Errorf("footer: %q", f)
	}
	press(m, "/", "x", "esc") // esc restores the previous filter
	if m.rowFilter != "gadget" {
		t.Errorf("filter = %q after esc, want it restored", m.rowFilter)
	}
	press(m, "/")
	for range 6 {
		press(m, "backspace")
	}
	press(m, "enter")

	// s cycles: panel setting, now descending, now ascending, label.
	order := func() []string {
		var out []string
		for _, r := range m.panels[0].shown {
			out = append(out, r.Title)
		}
		return out
	}
	press(m, "s")
	if got := strings.Join(order(), ","); got != "database-controller,gadget-controller,widget-controller" {
		t.Errorf("now desc: %s", got)
	}
	press(m, "s")
	if got := strings.Join(order(), ","); got != "widget-controller,gadget-controller,database-controller" {
		t.Errorf("now asc: %s", got)
	}
	press(m, "s")
	if got := strings.Join(order(), ","); got != "database-controller,gadget-controller,widget-controller" {
		t.Errorf("label: %s", got)
	}
	press(m, "s")
	if m.sortMode != 0 || !strings.Contains(m.renderFooter(""), "Sort (s): panel") {
		t.Errorf("sort did not return to the panel setting: %q", m.renderFooter(""))
	}
}
