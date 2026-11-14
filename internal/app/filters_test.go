package app

import "testing"

func TestMatchesFiltersAppliesTheSameRulesToBothViews(t *testing.T) {
	labels := map[string]string{"handler": "/api", "env": "prod"}

	cases := []struct {
		name        string
		metric      string
		label       string
		seriesName  string
		wantMatched bool
	}{
		{"no filters admits everything", "", "", "anything", true},
		{"metric regex matches", "^lat_", "", "lat_seconds", true},
		{"metric regex rejects", "^lat_", "", "rpc_seconds", false},
		{"exact label matches", "", "env=prod", "lat_seconds", true},
		{"exact label mismatch", "", "env=dev", "lat_seconds", false},
		{"absent label key rejects", "", "zone=eu", "lat_seconds", false},
		{"label regex matches", "", "handler=~^/api", "lat_seconds", true},
		{"label regex rejects", "", "handler=~^/ui", "lat_seconds", false},
		{"bare regex matches any value", "", "^/api$", "lat_seconds", true},
		{"bare regex matching nothing rejects", "", "^/nope$", "lat_seconds", false},
		{"both filters must pass", "^lat_", "env=dev", "lat_seconds", false},
		{"every clause must pass", "", "env=prod,handler=/api", "lat_seconds", true},
		{"one failing clause rejects", "", "env=prod,handler=/ui", "lat_seconds", false},
		{"a negated clause holds", "", "env=prod,handler!=/ui", "lat_seconds", true},
		{"a negated clause rejects", "", "env!=prod", "lat_seconds", false},
		{"a negated regex", "", "handler!~^/ui,env=~pro", "lat_seconds", true},
		{"an absent label satisfies a negation", "", "zone!=eu", "lat_seconds", true},
		{"an absent label fails a positive clause", "", "zone=eu,env=prod", "lat_seconds", false},
		{"a keyed and a bare clause", "", "env=prod,^/api$", "lat_seconds", true},
		{"a comma inside a repetition is not a separator", "", "env=~pro{1,2}d", "lat_seconds", true},
		{"the metric filter still applies alongside clauses", "^rpc_", "env=prod,handler=/api", "lat_seconds", false},
	}
	for _, tc := range cases {
		m := model{cfg: Config{FilterMetric: tc.metric, FilterLabel: tc.label}}
		if got := m.matchesFilters(tc.seriesName, labels); got != tc.wantMatched {
			t.Errorf("%s: matchesFilters = %v, want %v", tc.name, got, tc.wantMatched)
		}
	}
}
