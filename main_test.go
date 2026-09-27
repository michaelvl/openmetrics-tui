package main

import (
	"strings"
	"testing"
	"time"

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
	// The two leftmost columns have no scrape yet and fall back to the interval.
	want := []string{"-25s", "-20s", "-2m40s", "-2m35s", "-5s", "Curr"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("headers = %v, want %v", got, want)
	}
}

func TestFormatAge(t *testing.T) {
	cases := map[time.Duration]string{
		4600 * time.Millisecond:                   "5s",
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
