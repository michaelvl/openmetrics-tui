package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Headers read the scrape log, so an outage or a pause shows as a jump between
// neighbouring columns rather than being hidden behind evenly spaced labels.
func TestColumnHeadersShowRealScrapeAges(t *testing.T) {
	store := NewStore(10)
	t0 := time.Unix(1000, 0)
	for _, at := range []time.Duration{0, 5 * time.Second, 155 * time.Second, 160 * time.Second} {
		store.UpdateFromFamilies(parseFamilies(t, "# TYPE g gauge\ng 1\n"), t0.Add(at))
	}
	m := model{cfg: Config{Interval: 5 * time.Second}, store: store}

	got := m.columnHeaders(6)
	// The two leftmost columns have no scrape yet. They carry on back from the
	// oldest real scrape in steps of the interval, so they never read younger
	// than the column beside them.
	want := []string{"-2m50s", "-2m45s", "-2m40s", "-2m35s", "-5s", "Curr"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("headers = %v, want %v", got, want)
	}
}

func TestFormatAge(t *testing.T) {
	cases := map[time.Duration]string{
		250 * time.Millisecond:                    "250ms",
		998 * time.Millisecond:                    "1s",
		1520 * time.Millisecond:                   "1.5s",
		4600 * time.Millisecond:                   "4.6s",
		5030 * time.Millisecond:                   "5s",
		12400 * time.Millisecond:                  "12s",
		90 * time.Second:                          "1m30s",
		2 * time.Minute:                           "2m",
		time.Hour + 5*time.Minute + 3*time.Second: "1h5m",
		3 * time.Hour:                             "3h",
	}
	for d, want := range cases {
		if got := formatAge(d); got != want {
			t.Errorf("formatAge(%v) = %q, want %q", d, got, want)
		}
	}
}

// Columns from before the latest break are drawn in the alternate style; the
// newest epoch keeps the usual one.
func TestOlderEpochColumnsUseTheAlternateStyle(t *testing.T) {
	// Without a terminal lipgloss renders no colour, which would make the
	// comparison below pass whatever style was picked.
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })

	store := NewStore(10)
	store.UpdateFromFamilies(parseFamilies(t, "# TYPE g gauge\ng 7\n"), time.Unix(0, 0))
	store.MarkBreak()
	store.UpdateFromFamilies(parseFamilies(t, "# TYPE g gauge\ng 8\n"), time.Unix(60, 0))
	store.UpdateFromFamilies(parseFamilies(t, "# TYPE g gauge\ng 9\n"), time.Unix(65, 0))

	m := model{cfg: Config{History: 3, DeltaMode: DeltaModeOff, LabelMode: LabelModeHideAll}, store: store}
	row := m.buildTableRows([]*MetricSeries{store.Metrics[`g{}`]})[0]
	if row[1] != epochValueStyle.Render("7") {
		t.Errorf("pre-break cell = %q, want it in the epoch style", row[1])
	}
	if row[2] != "8" {
		t.Errorf("post-break history cell = %q, want it unstyled", row[2])
	}
}

func TestStepIntervalWalksTheLadder(t *testing.T) {
	cases := []struct {
		cur    time.Duration
		faster bool
		want   time.Duration
	}{
		{5 * time.Second, true, 2 * time.Second},
		{5 * time.Second, false, 10 * time.Second},
		// An interval from the command line that is off the ladder joins it at
		// the nearest step in the asked direction.
		{3 * time.Second, true, 2 * time.Second},
		{3 * time.Second, false, 5 * time.Second},
		// Both ends hold.
		{250 * time.Millisecond, true, 250 * time.Millisecond},
		{5 * time.Minute, false, 5 * time.Minute},
		{10 * time.Minute, false, 10 * time.Minute},
	}
	for _, tc := range cases {
		if got := stepInterval(tc.cur, tc.faster); got != tc.want {
			t.Errorf("stepInterval(%v, faster=%v) = %v, want %v", tc.cur, tc.faster, got, tc.want)
		}
	}
}

// Changing the interval starts a fresh tick chain, and a tick from the old one
// must end it rather than keep a second chain running.
func TestIntervalChangeRetiresTheOldTickChain(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	oldGen := m.tickGen

	m, cmd := press(m, "+")
	if m.cfg.Interval != 2*time.Second {
		t.Fatalf("interval = %v after +, want 2s", m.cfg.Interval)
	}
	if cmd == nil || m.tickGen == oldGen {
		t.Fatalf("+ should start a new tick chain (gen %d -> %d, cmd %v)", oldGen, m.tickGen, cmd)
	}

	next, cmd := m.Update(tickMsg{gen: oldGen})
	if cmd != nil {
		t.Error("a tick from the retired chain scheduled more work")
	}
	if next.(model).fetching {
		t.Error("a tick from the retired chain started a fetch")
	}

	m, _ = press(m, "-")
	if m.cfg.Interval != 5*time.Second {
		t.Errorf("interval = %v after -, want back to 5s", m.cfg.Interval)
	}
}

// A tick that finds the previous fetch still out skips its fetch, so a short
// interval against a slow target cannot pile them up.
func TestTickSkipsTheFetchWhileOneIsOut(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	// The tick command below is run, and it sleeps for the interval.
	m.cfg.Interval = time.Millisecond

	next, _ := m.Update(tickMsg{gen: m.tickGen})
	m = next.(model)
	if !m.fetching {
		t.Fatal("a tick should start a fetch")
	}

	next, cmd := m.Update(tickMsg{gen: m.tickGen})
	if cmd == nil {
		t.Fatal("the tick chain stopped while a fetch was out")
	}
	// A tick that fetches returns a batch of fetch and next tick; one that skips
	// the fetch returns the next tick alone.
	if _, isBatch := cmd().(tea.BatchMsg); isBatch {
		t.Error("a second fetch was issued while one was out")
	}

	next, _ = next.(model).Update(scrapeResult{families: parseFamilies(t, twoGauges), at: time.Now()})
	if next.(model).fetching {
		t.Error("a completed fetch left the flag set")
	}
}

// - is also the interval key, but while a header box is open it is text.
func TestMinusTypesIntoAnOpenFilter(t *testing.T) {
	m := headerModel(t, 80, twoGauges)
	m, _ = press(m, "m")
	m = typeKeys(m, "a-b")
	if got := m.input.Value(); got != "a-b" {
		t.Errorf("filter input = %q, want a-b", got)
	}
	if m.cfg.Interval != 5*time.Second {
		t.Errorf("interval changed to %v while editing", m.cfg.Interval)
	}
}
