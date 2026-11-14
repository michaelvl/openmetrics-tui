package app

import (
	"regexp"

	"github.com/michaelvl/openmetrics-tui/internal/filter"
)

// matchesFilters reports whether a series passes the -filter-metric and
// -filter-label options. Both views share it, so a filter means the same thing
// whether it is applied to a gauge or to a histogram family.
func (m model) matchesFilters(name string, labels map[string]string) bool {
	if m.cfg.FilterMetric != "" {
		matched, _ := regexp.MatchString(m.cfg.FilterMetric, name)
		if !matched {
			return false
		}
	}
	return filter.MatchesLabelFilter(m.cfg.FilterLabel, labels)
}
