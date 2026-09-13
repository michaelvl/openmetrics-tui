package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// twoGauges gives the label filter something to bite on: one series per env.
const twoGauges = `# TYPE queue_depth gauge
queue_depth{env="prod"} 12
queue_depth{env="dev"} 3
# TYPE worker_count gauge
worker_count{env="prod"} 4
`

// fourGauges has two label dimensions, so a filter needs both of its clauses to
// pick out a single series.
const fourGauges = `# TYPE queue_depth gauge
queue_depth{env="prod",region="eu"} 12
queue_depth{env="prod",region="us"} 7
queue_depth{env="dev",region="eu"} 3
queue_depth{env="dev",region="us"} 1
`

// headerModel builds a model in the metrics view, wired the way main() wires
// the real one, so a test drives the same Update path the keyboard does.
func headerModel(t *testing.T, width int, texts ...string) model {
	t.Helper()
	store := NewStore(10)
	for _, text := range texts {
		store.UpdateFromFamilies(parseFamilies(t, text))
	}

	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 256
	input.Cursor.SetMode(cursor.CursorStatic)

	m := model{
		cfg: Config{
			Interval:  5 * time.Second,
			History:   10,
			LabelMode: LabelModeShowAll,
			DeltaMode: DeltaModeOff,
		},
		store:         store,
		width:         width,
		height:        24,
		view:          ViewMetrics,
		bucketMode:    BucketModePerBucket,
		expanded:      make(map[string]bool),
		viewport:      viewport.New(width, 20),
		viewportReady: true,
		input:         input,
	}
	m.refresh()
	return m
}

// press feeds one key through Update, the way bubbletea would.
func press(m model, key string) (model, tea.Cmd) {
	var msg tea.KeyMsg
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		msg = tea.KeyMsg{Type: tea.KeyShiftTab}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, cmd := m.Update(msg)
	return next.(model), cmd
}

// typeKeys presses each rune of s in turn.
func typeKeys(m model, s string) model {
	for _, r := range s {
		m, _ = press(m, string(r))
	}
	return m
}

func TestFOpensTheLabelFieldSeededWithTheCurrentFilter(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	m.cfg.FilterLabel = "env=prod"

	m, _ = press(m, "f")
	if m.editing != fieldLabelFilter {
		t.Fatalf("editing = %d after f, want fieldLabelFilter", m.editing)
	}
	if got := m.input.Value(); got != "env=prod" {
		t.Errorf("input = %q, want the filter in force", got)
	}
}

func TestEscapeDiscardsAnEditAndEnterAppliesIt(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	m.cfg.FilterLabel = "env=prod"

	m, _ = press(m, "f")
	m = typeKeys(m, "!")
	m, _ = press(m, "esc")
	if m.editing != fieldNone {
		t.Errorf("esc left the field focused")
	}
	if m.cfg.FilterLabel != "env=prod" {
		t.Errorf("FilterLabel = %q after esc, want it untouched", m.cfg.FilterLabel)
	}

	// Now apply a different filter and watch the table follow.
	m, _ = press(m, "f")
	for range "env=prod" {
		m, _ = press(m, "backspace")
	}
	m = typeKeys(m, "env=dev")
	m, _ = press(m, "enter")

	if m.editing != fieldNone {
		t.Errorf("enter left the field focused")
	}
	if m.cfg.FilterLabel != "env=dev" {
		t.Fatalf("FilterLabel = %q, want env=dev", m.cfg.FilterLabel)
	}
	table := plain(m.buildTable())
	if !strings.Contains(table, "env=dev") {
		t.Errorf("table does not show the dev series:\n%s", table)
	}
	if strings.Contains(table, "env=prod") {
		t.Errorf("table still shows a prod series after filtering to dev:\n%s", table)
	}
}

// The global keys are single letters, so an unguarded filter box would quit the
// program halfway through typing a metric name.
func TestGlobalKeysAreInertWhileEditing(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	m, _ = press(m, "f")

	before := m.cfg
	for _, key := range []string{"q", "l", "d", "p", "s", "v"} {
		var cmd tea.Cmd
		m, cmd = press(m, key)
		if cmd != nil {
			t.Errorf("key %q while editing produced a command", key)
		}
	}

	if got := m.input.Value(); got != "qldpsv" {
		t.Errorf("input = %q, want the keys typed as text", got)
	}
	if m.cfg != before {
		t.Errorf("cfg changed while editing: %+v -> %+v", before, m.cfg)
	}
	if m.view != ViewMetrics {
		t.Errorf("v switched the view while editing")
	}
}

func TestInvalidRegexKeepsFocusAndLeavesTheFilterAlone(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	m.cfg.FilterMetric = "queue"

	m, _ = press(m, "m")
	m = typeKeys(m, "_(")
	m, _ = press(m, "enter")

	if m.editing != fieldMetricFilter {
		t.Fatalf("a bad regex let go of the keyboard")
	}
	if m.inputErr == "" {
		t.Errorf("no error recorded for %q", m.input.Value())
	}
	if m.cfg.FilterMetric != "queue" {
		t.Errorf("FilterMetric = %q, want the old value kept", m.cfg.FilterMetric)
	}

	// Typing again is the user fixing it, so the complaint goes away before they
	// have even committed.
	m, _ = press(m, "backspace")
	if m.inputErr != "" {
		t.Errorf("inputErr = %q, want it cleared by the next keystroke", m.inputErr)
	}
	m, _ = press(m, "enter")
	if m.editing != fieldNone || m.cfg.FilterMetric != "queue_" {
		t.Errorf("editing = %d, FilterMetric = %q, want the fixed pattern applied",
			m.editing, m.cfg.FilterMetric)
	}
}

// A "key=value" label filter is not a regex at all, so its value must not be
// held to regex syntax.
func TestLabelFilterValidationFollowsItsClauseForms(t *testing.T) {
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"", false},
		{"env=prod(1)", false}, // exact match, never compiled
		{"env=~pro.*", false},
		{"env=~pro(", true},
		{"env!~pro(", true},
		{"pro.*", false},
		{"pro(", true},
		{"env=prod,region=eu", false},
		{"env=prod,region!=eu", false},
		{"env=prod,region=~eu(", true}, // a later clause is checked too
		{"env=prod,", true},            // a trailing comma is an empty clause
		{",env=prod", true},
		{"env=prod,,region=eu", true},
		{"=prod", true},          // an operator with no label name
		{"env=~pro{1,2}", false}, // the comma belongs to the repetition
	}
	for _, tc := range tests {
		err := validateLabelFilter(tc.value)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateLabelFilter(%q) = %v, wantErr %v", tc.value, err, tc.wantErr)
		}
	}
}

func TestTabWalksTheFieldsApplyingEachOne(t *testing.T) {
	m := headerModel(t, 80, twoGauges)

	m, _ = press(m, "m")
	m = typeKeys(m, "queue")
	m, _ = press(m, "tab")
	if m.editing != fieldLabelFilter {
		t.Fatalf("tab from the metric field landed on %d, want the label field", m.editing)
	}
	if m.cfg.FilterMetric != "queue" {
		t.Errorf("FilterMetric = %q, want tab to have applied it", m.cfg.FilterMetric)
	}

	m = typeKeys(m, "env=prod")
	m, _ = press(m, "tab")
	if m.editing != fieldAggregation {
		t.Fatalf("tab landed on %d, want the aggregation field", m.editing)
	}
	if m.cfg.FilterLabel != "env=prod" {
		t.Errorf("FilterLabel = %q, want tab to have applied it", m.cfg.FilterLabel)
	}

	// The tab order wraps, and shift+tab walks it backwards.
	m, _ = press(m, "tab")
	if m.editing != fieldMetricFilter {
		t.Errorf("tab from the last field landed on %d, want it to wrap", m.editing)
	}
	m, _ = press(m, "shift+tab")
	if m.editing != fieldAggregation {
		t.Errorf("shift+tab landed on %d, want the aggregation field", m.editing)
	}
}

// A tab out of a broken field would put the error off screen, so it has to stay.
func TestTabIsRefusedByABrokenField(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	m, _ = press(m, "m")
	m = typeKeys(m, "queue_(")
	m, _ = press(m, "tab")

	if m.editing != fieldMetricFilter {
		t.Errorf("tab escaped a field holding %q", m.input.Value())
	}
	if m.inputErr == "" {
		t.Errorf("no error recorded")
	}
}

func TestAggregationAppliesOnCommit(t *testing.T) {
	m := headerModel(t, 80, fourGauges)

	m, _ = press(m, "a")
	m = typeKeys(m, "env")
	m, _ = press(m, "enter")

	if m.cfg.Aggregation != "env" {
		t.Fatalf("Aggregation = %q, want it stored", m.cfg.Aggregation)
	}
	if !strings.Contains(plain(m.renderHeader()), "env") {
		t.Errorf("header does not show the aggregation:\n%s", plain(m.renderHeader()))
	}

	// The four series collapse to one row per env, and the row names itself as
	// derived - which is what tells the reader the 19 below is a sum.
	table := plain(m.buildTable())
	for _, want := range []string{"sum(queue_depth){env=prod}", "19", "sum(queue_depth){env=dev}", "4"} {
		if !strings.Contains(table, want) {
			t.Errorf("aggregated table is missing %q:\n%s", want, table)
		}
	}
	if strings.Contains(table, "region") {
		t.Errorf("aggregated away label still shown:\n%s", table)
	}
}

// The PromQL spelling is the likeliest first attempt, so it has to land on the
// same grouping the bare list does rather than on an error.
func TestAggregationAcceptsThePromQLSpelling(t *testing.T) {
	bare := headerModel(t, 80, fourGauges)
	bare.cfg.Aggregation = "env"
	want := plain(bare.buildTable())

	for _, typed := range []string{"sum by (env)", "sum by(env)", "by (env)", "sum by env"} {
		m := headerModel(t, 80, fourGauges)
		m, _ = press(m, "a")
		m = typeKeys(m, typed)
		m, _ = press(m, "enter")

		if m.editing != fieldNone {
			t.Errorf("%q was refused: %s", typed, m.inputErr)
			continue
		}
		if got := plain(m.buildTable()); got != want {
			t.Errorf("%q did not group the same as the bare list:\n%s\nwant:\n%s", typed, got, want)
		}
	}
}

// "without" names the labels to drop rather than the ones to keep, so waving it
// through would group by the exact complement of what was asked for.
func TestAggregationRefusesWithout(t *testing.T) {
	m := headerModel(t, 80, fourGauges)
	before := plain(m.buildTable())

	m, _ = press(m, "a")
	m = typeKeys(m, "sum without (region)")
	m, _ = press(m, "enter")

	if m.editing != fieldAggregation {
		t.Errorf("enter left a broken aggregation field")
	}
	if m.inputErr == "" {
		t.Errorf("no error recorded for %q", m.input.Value())
	}
	if m.cfg.Aggregation != "" {
		t.Errorf("Aggregation = %q, want it left unapplied", m.cfg.Aggregation)
	}
	if got := plain(m.buildTable()); got != before {
		t.Errorf("a refused aggregation changed the table:\n%s", got)
	}
}

// The header's height is what the viewport is sized against, so a wrapped box
// would push the bottom row off screen.
func TestHeaderIsAlwaysThreeRowsAndFitsTheTerminal(t *testing.T) {
	withColor(t)
	for _, width := range []int{120, 80, 60, 50, 40, 30, 20, 12} {
		m := headerModel(t, width, twoGauges)
		m.cfg.FilterMetric = "a_very_long_metric_name_pattern_.*"
		m.cfg.FilterLabel = "environment=~production|staging"

		for _, field := range append([]headerField{fieldNone}, editableFields...) {
			if field != fieldNone {
				m.startEditing(field)
			} else {
				m.cancelEditing()
			}
			out := m.renderHeader()
			if got := lipgloss.Height(out); got != headerHeight {
				t.Errorf("width %d, field %d: header is %d rows, want %d:\n%s",
					width, field, got, headerHeight, plain(out))
			}
			if got := lipgloss.Width(out); got > width {
				t.Errorf("width %d, field %d: header is %d columns wide:\n%s",
					width, field, got, plain(out))
			}
		}
	}
}

func TestHeaderShowsTheFiltersInForce(t *testing.T) {
	m := headerModel(t, 120, twoGauges)
	m.cfg.FilterMetric = "queue_.*"
	m.cfg.FilterLabel = "env=prod"

	got := plain(m.renderHeader())
	for _, want := range []string{"Metric:", "queue_.*", "Label:", "env=prod", "Aggregation:"} {
		if !strings.Contains(got, want) {
			t.Errorf("header is missing %q:\n%s", want, got)
		}
	}
}

// A resize has to leave room for the header as well as the footer, or the last
// table row lands underneath it.
func TestResizeReservesTheHeadersRows(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(model)

	if want := 40 - headerHeight - 2; m.viewport.Height != want {
		t.Errorf("viewport height = %d, want %d", m.viewport.Height, want)
	}
	if lipgloss.Height(m.View()) > 40 {
		t.Errorf("view is %d rows, want at most 40", lipgloss.Height(m.View()))
	}
}

// The footer's left segment carries the edit hint, since the header is a fixed
// height with no room for one.
func TestFooterCarriesTheEditHint(t *testing.T) {
	m := headerModel(t, 120, twoGauges)
	if strings.Contains(plain(m.View()), "esc: cancel") {
		t.Errorf("hint shown while nothing is being edited")
	}

	m, _ = press(m, "f")
	if !strings.Contains(plain(m.View()), "esc: cancel") {
		t.Errorf("no edit hint while editing:\n%s", plain(m.View()))
	}

	m = typeKeys(m, "env=~pro(")
	m, _ = press(m, "enter")
	if !strings.Contains(plain(m.View()), "missing closing )") {
		t.Errorf("footer does not carry the validation error:\n%s", plain(m.View()))
	}
}

// The hint and a regex error share the footer's one line with the endpoint URL,
// and a wrapped footer costs a row of the table. Below 60 columns the footer's
// own fixed segments already overflow on their own - that predates the header
// and is not what this is testing - so the widths here are the ones where idle
// fits and editing therefore has to fit too.
func TestFooterFitsTheTerminalWhileEditing(t *testing.T) {
	withColor(t)
	const longURL = "http://metrics.internal.example.com:9090/federate/exporters/node"
	for _, width := range []int{120, 100, 80, 60} {
		m := headerModel(t, width, twoGauges)
		m.cfg.URL = longURL
		m.isConnected = true
		next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m = next.(model)

		check := func(state string, m model) {
			t.Helper()
			out := m.View()
			if got := lipgloss.Width(out); got > width {
				t.Errorf("width %d, %s: view is %d columns:\n%s", width, state, got, plain(out))
			}
			if got := lipgloss.Height(out); got > 24 {
				t.Errorf("width %d, %s: view is %d rows", width, state, got)
			}
		}
		check("idle", m)

		m, _ = press(m, "m")
		check("editing", m)

		// A regex error is as long as whatever the user typed, so it is the case
		// most likely to run off the end of the line.
		m = typeKeys(m, "a_very_long_pattern_that_runs_on_and_on_(")
		m, _ = press(m, "enter")
		if m.inputErr == "" {
			t.Fatalf("width %d: expected a validation error", width)
		}
		check("error", m)
		if !strings.Contains(plain(m.View()), "missing closing )") {
			t.Errorf("width %d: the error was truncated away:\n%s", width, plain(m.View()))
		}
	}
}

// The distribution view's footer carries one segment more than the metrics
// view's, and a wrapped footer costs a row of the grid. Its own floor is higher
// than the metrics view's because "v: Distributions" is the longer label, so the
// widths here start where that label already fits; what this pins is that the
// bucket indicator gives way rather than pushing the line over the edge.
func TestDistributionFooterFitsTheTerminal(t *testing.T) {
	withColor(t)
	const longURL = "http://metrics.internal.example.com:9090/federate/exporters/node"
	for _, width := range []int{120, 100, 80, 70} {
		m := distModel(t, width, threeFamilies)
		m.cfg.URL = longURL
		m.isConnected = true
		next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m = next.(model)
		m.view = ViewDistributions
		m.refresh()

		out := m.View()
		if got := lipgloss.Width(out); got > width {
			t.Errorf("width %d: view is %d columns:\n%s", width, got, plain(out))
		}
		// Where there is room, the two axes have to be readable together - that is
		// the whole point of moving the bucket mode down beside the delta mode.
		if width >= 100 && !strings.Contains(plain(out), "Buckets: Per bucket") {
			t.Errorf("width %d: the footer lost the bucket mode:\n%s", width, plain(out))
		}
	}
}

// The useful half of a regexp error is the complaint; the other half repeats the
// pattern, which is already on screen in the box above.
func TestRegexErrorsDropTheirBoilerplate(t *testing.T) {
	err := validateMetricFilter("queue_(")
	if err == nil {
		t.Fatal("queue_( compiled")
	}
	if got := err.Error(); got != "missing closing )" {
		t.Errorf("error = %q, want just the complaint", got)
	}
}

// A filter typed in the distribution view has to leave the cursor on a row that
// still exists, the same way TestCursorFollowsAShrinkingList checks for a filter
// set directly on the config.
func TestApplyingAFilterClampsTheDistributionCursor(t *testing.T) {
	m := distModel(t, 120, threeFamilies)
	m.input = textinput.New()
	m.input.Prompt = ""
	m.moveCursor(2)
	if m.distCursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.distCursor)
	}

	m, _ = press(m, "m")
	m = typeKeys(m, "^a_")
	m, _ = press(m, "enter")

	if m.cfg.FilterMetric != "^a_" {
		t.Fatalf("FilterMetric = %q, want ^a_", m.cfg.FilterMetric)
	}
	if m.distCursor != 0 {
		t.Errorf("cursor = %d after filtering down to one row, want 0", m.distCursor)
	}
}

func TestATwoClauseFilterNarrowsToSeriesMatchingBoth(t *testing.T) {
	m := headerModel(t, 80, fourGauges)

	m, _ = press(m, "f")
	m = typeKeys(m, "env=prod,region=eu")
	m, _ = press(m, "enter")

	if m.editing != fieldNone {
		t.Fatalf("enter left the field focused, inputErr = %q", m.inputErr)
	}
	table := plain(m.buildTable())
	if !strings.Contains(table, "env=prod") || !strings.Contains(table, "region=eu") {
		t.Errorf("table dropped the series matching both clauses:\n%s", table)
	}
	for _, gone := range []string{"region=us", "env=dev"} {
		if strings.Contains(table, gone) {
			t.Errorf("table still shows %s after filtering to prod and eu:\n%s", gone, table)
		}
	}

	// Negating the second clause picks out the other prod series instead.
	m, _ = press(m, "f")
	for range m.cfg.FilterLabel {
		m, _ = press(m, "backspace")
	}
	m = typeKeys(m, "env=prod,region!=eu")
	m, _ = press(m, "enter")

	table = plain(m.buildTable())
	if !strings.Contains(table, "region=us") {
		t.Errorf("table dropped the us series:\n%s", table)
	}
	if strings.Contains(table, "region=eu") {
		t.Errorf("table still shows the eu series after negating it:\n%s", table)
	}
}

// Hide-filtered drops the columns the filter already told you about, and only
// those: a negated clause leaves the value open, so its label stays.
func TestHideFilteredDropsOnlyThePinnedLabels(t *testing.T) {
	m := headerModel(t, 80, fourGauges)
	m.cfg.FilterLabel = "env=prod,region!=eu"
	m.cfg.LabelMode = LabelModeHideFiltered
	m.refresh()

	table := plain(m.buildTable())
	if strings.Contains(table, "env=prod") {
		t.Errorf("hide-filtered kept the pinned env label:\n%s", table)
	}
	if !strings.Contains(table, "region=us") {
		t.Errorf("hide-filtered dropped the negated region label:\n%s", table)
	}
}

// The hide-filtered mode is only worth offering when the filter names a label.
func TestLabelModeCycleSkipsHideFilteredWithoutAPinnedLabel(t *testing.T) {
	m := headerModel(t, 80, fourGauges)
	m.cfg.FilterLabel = "prod" // a bare regex, naming no label

	m, _ = press(m, "l")
	if m.cfg.LabelMode != LabelModeHideAll {
		t.Errorf("LabelMode = %q, want the cycle to skip hide-filtered", m.cfg.LabelMode)
	}

	m.cfg.FilterLabel = "env=prod"
	m.cfg.LabelMode = LabelModeShowAll
	m, _ = press(m, "l")
	if m.cfg.LabelMode != LabelModeHideFiltered {
		t.Errorf("LabelMode = %q, want hide-filtered once a label is pinned", m.cfg.LabelMode)
	}
}
