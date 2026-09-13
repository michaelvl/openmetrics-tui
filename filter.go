package main

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
)

// The label filter is a comma-separated list of clauses, all of which must
// match for a series to be shown. A clause is one of:
//
//	env=prod        the label equals a value
//	env!=dev        the label does not equal a value
//	env=~prod|stg   the label matches a regex
//	env!~dev|test   the label does not match a regex
//	prod            a regex matched against every label value in turn
//
// A label the series does not carry reads as the empty string, the way PromQL
// treats it. That is what makes negation useful: env!=dev admits a series with
// no env label at all, which is almost always what someone asking for "not dev"
// means. It also makes "env=" a way to ask for series lacking the label.

// clauseOp is the comparison a clause makes. opNone is the bare-regex form,
// which has no label name to compare against.
type clauseOp int

const (
	opNone clauseOp = iota
	opEq
	opNe
	opRe
	opNre
)

// splitFilterClauses breaks a filter into its clauses.
//
// The separator cannot simply be strings.Split: clause values are regexes, and
// a regex may hold a comma of its own - "env=~prod{1,2}" is one clause, not a
// torn pair of invalid patterns. So commas inside a {} repetition or a []
// character class are left alone, and "\," is an escape that yields a literal
// comma in the clause.
func splitFilterClauses(filter string) []string {
	if filter == "" {
		return nil
	}

	var clauses []string
	var buf strings.Builder
	depth := 0       // open { } repetitions
	inClass := false // inside a [ ] character class
	// classStart is where a ] would still be a literal rather than the closing
	// bracket, which is the first position in the class - "[]]" matches "]".
	classStart := 0

	for i := 0; i < len(filter); i++ {
		c := filter[i]
		switch {
		case c == '\\':
			if i+1 >= len(filter) {
				buf.WriteByte(c)
				continue
			}
			i++
			if filter[i] == ',' {
				// The separator's own escape: the comma is data, and the
				// backslash has done its job and does not belong in the clause.
				buf.WriteByte(',')
				continue
			}
			// Any other escape is the regex's business, so it passes through
			// whole.
			buf.WriteByte(c)
			buf.WriteByte(filter[i])
		case inClass:
			if c == ']' && i != classStart {
				inClass = false
			}
			buf.WriteByte(c)
		case c == '[':
			inClass = true
			classStart = i + 1
			if classStart < len(filter) && filter[classStart] == '^' {
				classStart++
			}
			buf.WriteByte(c)
		case c == '{':
			depth++
			buf.WriteByte(c)
		case c == '}':
			if depth > 0 {
				depth--
			}
			buf.WriteByte(c)
		case c == ',' && depth == 0:
			clauses = append(clauses, strings.TrimSpace(buf.String()))
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	return append(clauses, strings.TrimSpace(buf.String()))
}

// parseLabelClause splits one clause into its label name, comparison and value.
// The operator is the first of "!~", "!=", "=~", "=" to appear, so a value may
// hold an = of its own - "url=~http://x?a=1" compares the url label. A "!" that
// begins neither "!~" nor "!=" is an ordinary character, and a clause with no
// operator at all is a bare regex, reported as opNone with no label name.
func parseLabelClause(clause string) (key string, op clauseOp, value string) {
	for i := 0; i < len(clause); i++ {
		width := 2
		switch clause[i] {
		case '!':
			if i+1 >= len(clause) {
				continue
			}
			switch clause[i+1] {
			case '~':
				op = opNre
			case '=':
				op = opNe
			default:
				continue
			}
		case '=':
			if i+1 < len(clause) && clause[i+1] == '~' {
				op = opRe
			} else {
				op, width = opEq, 1
			}
		default:
			continue
		}
		return clause[:i], op, clause[i+width:]
	}
	return "", opNone, clause
}

// matchesLabelFilter reports whether a series' labels satisfy every clause. An
// empty filter has no clauses and so admits everything.
func matchesLabelFilter(filter string, labels map[string]string) bool {
	for _, clause := range splitFilterClauses(filter) {
		if !matchesLabelClause(clause, labels) {
			return false
		}
	}
	return true
}

// matchesLabelClause reports whether one clause holds.
func matchesLabelClause(clause string, labels map[string]string) bool {
	key, op, value := parseLabelClause(clause)
	if op == opNone {
		// A clause naming no label is tried against every label in turn, and one
		// match is enough. This is the only form that is an or rather than an and.
		for _, v := range labels {
			if ok, _ := regexp.MatchString(clause, v); ok {
				return true
			}
		}
		return false
	}

	// A label the series does not carry reads as empty rather than as an
	// automatic mismatch; see the note at the top of the file.
	val := labels[key]

	switch op {
	case opEq:
		return val == value
	case opNe:
		return val != value
	case opRe:
		matched, _ := regexp.MatchString(value, val)
		return matched
	case opNre:
		matched, _ := regexp.MatchString(value, val)
		return !matched
	}
	return true
}

// getFilteredLabelKeys is the set of label names the filter pins down, which is
// what LabelModeHideFiltered drops from the display: there is no point spending
// columns on a value the filter already told you.
//
// A negated clause contributes nothing, because it does not pin the value down -
// "env!=dev" leaves prod and staging both possible, so the column still carries
// information. Neither does a bare regex, which names no label at all.
func getFilteredLabelKeys(filterLabel string) []string {
	keys := []string{}
	for _, clause := range splitFilterClauses(filterLabel) {
		key, op, _ := parseLabelClause(clause)
		if key == "" || op == opNone || op == opNe || op == opNre {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

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
	return matchesLabelFilter(m.cfg.FilterLabel, labels)
}

// validateMetricFilter rejects a pattern that matchesFilters would silently
// treat as matching nothing.
func validateMetricFilter(value string) error {
	if value == "" {
		return nil
	}
	_, err := regexp.Compile(value)
	return compileError(err)
}

// validateLabelFilter rejects a filter that matchesLabelFilter would silently
// treat as matching nothing, checking every clause rather than only the first.
func validateLabelFilter(value string) error {
	clauses := splitFilterClauses(value)
	for _, clause := range clauses {
		err := validateLabelClause(clause)
		if err == nil {
			continue
		}
		if len(clauses) == 1 {
			return err
		}
		return clauseError(clause, err)
	}
	return nil
}

func validateLabelClause(clause string) error {
	if clause == "" {
		// A stray or trailing comma. Ignoring it would be the silent no-op that
		// a comma used to be before clauses existed.
		return errors.New("empty filter clause")
	}
	key, op, value := parseLabelClause(clause)
	if op == opNone {
		_, err := regexp.Compile(clause)
		return compileError(err)
	}
	if key == "" {
		return errors.New("missing label name")
	}
	if op == opRe || op == opNre {
		_, err := regexp.Compile(value)
		return compileError(err)
	}
	return nil
}

// clauseError names the clause a complaint came from. With one clause the box
// above the footer already shows it; with several there is otherwise no way to
// tell which one is broken, and the footer has no room for the whole filter.
func clauseError(clause string, err error) error {
	key, op, _ := parseLabelClause(clause)
	if op == opNone || key == "" {
		key = truncateToWidth(clause, 12)
	}
	if key == "" {
		// An empty clause has nothing to name it by, and the complaint already
		// says what it is.
		return err
	}
	return fmt.Errorf("%s: %w", key, err)
}

// compileError trims what regexp.Compile says down to the part the user cannot
// already see. Its message repeats the whole pattern back - "error parsing
// regexp: missing closing ): `queue_(`" - and the pattern is sitting in the box
// right above the footer, where there is barely room for the complaint itself.
func compileError(err error) error {
	var rerr *syntax.Error
	if errors.As(err, &rerr) {
		return errors.New(string(rerr.Code))
	}
	return err
}
