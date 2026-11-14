package dashboard

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/prometheus/promql/parser"
)

// Matcher is one label matcher of a Selector. Value may still contain
// ${name} references to other variables.
type Matcher struct {
	Label string
	Op    string // "=", "!=", "=~" or "!~"
	Value string
}

// Selector says that Variable is matched against Label (with Op, "=" or "=~")
// in a vector selector: Metric plus Matchers, which are the other matchers of
// that selector. It is the raw material for a label_values lookup.
type Selector struct {
	Variable string
	Label    string
	Op       string
	Metric   string
	Matchers []Matcher
}

var wholeRefRe = regexp.MustCompile(`^\$\{([^}]*)\}$`)

const placeholderPrefix = "__omtui_var_"

// Selectors returns one Selector per vector selector in query that has an
// equality or regex matcher whose value is exactly a ${name} reference. Each
// reference is replaced by a placeholder token and the query is parsed, so
// variables in positions that cannot hold a placeholder (a range duration, a
// number) make the query unparseable and yield nil: those variables get no
// derived suggestions. A query that does not parse yields nil as well.
func Selectors(query string) []Selector {
	names := map[string]string{} // token -> variable name
	var order []string
	replaced := refRe.ReplaceAllStringFunc(query, func(m string) string {
		name := m[2 : len(m)-1]
		for tok, n := range names {
			if n == name {
				return tok
			}
		}
		tok := fmt.Sprintf("%s%d__", placeholderPrefix, len(names))
		names[tok] = name
		order = append(order, tok)
		return tok
	})
	expr, err := parser.ParseExpr(replaced)
	if err != nil {
		return nil
	}
	// restore turns placeholder tokens inside a matcher value back into ${name}.
	restore := func(s string) string {
		for _, tok := range order {
			s = strings.ReplaceAll(s, tok, "${"+names[tok]+"}")
		}
		return s
	}

	var out []Selector
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		vs, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		var all []Matcher
		for _, m := range vs.LabelMatchers {
			if m.Name == "__name__" && vs.Name != "" {
				continue
			}
			all = append(all, Matcher{Label: m.Name, Op: m.Type.String(), Value: restore(m.Value)})
		}
		for i, m := range all {
			if m.Op != "=" && m.Op != "=~" {
				continue
			}
			// Restored values are exactly "${name}" when the placeholder was the
			// whole value; embedded references give no derivation.
			sm := wholeRefRe.FindStringSubmatch(m.Value)
			if sm == nil {
				continue
			}
			name := sm[1]
			rest := make([]Matcher, 0, len(all)-1)
			rest = append(rest, all[:i]...)
			rest = append(rest, all[i+1:]...)
			out = append(out, Selector{Variable: name, Label: m.Label, Op: m.Op, Metric: vs.Name, Matchers: rest})
		}
		return nil
	})
	return out
}

// Lookup builds the label_values request for editing s.Variable: the label to
// list and the match[] selector ("" when nothing constrains the lookup). Every
// matcher that references the edited variable is dropped, so the list is not
// limited to the current value, and other variables are replaced by their
// current values from vals, so lists cascade. A referenced variable missing
// from vals is an error.
func (s Selector) Lookup(vals map[string]string) (label, match string, err error) {
	var parts []string
	for _, m := range s.Matchers {
		if mentions(m.Value, s.Variable) {
			continue
		}
		parts = append(parts, m.Label+m.Op+strconv.Quote(m.Value))
	}
	if s.Metric == "" && len(parts) == 0 {
		return s.Label, "", nil
	}
	sel := s.Metric
	if len(parts) > 0 || s.Metric == "" {
		sel += "{" + strings.Join(parts, ",") + "}"
	}
	match, err = Substitute(sel, vals)
	if err != nil {
		return "", "", err
	}
	return s.Label, match, nil
}

func mentions(value, variable string) bool {
	for _, r := range Refs(value) {
		if r == variable {
			return true
		}
	}
	return false
}

// SelectorsFor returns the selectors in queries that name variable, in query
// order with exact duplicates removed.
func SelectorsFor(variable string, queries []string) []Selector {
	var out []Selector
	seen := map[string]bool{}
	for _, q := range queries {
		for _, s := range Selectors(q) {
			if s.Variable != variable {
				continue
			}
			key := fmt.Sprintf("%v", s)
			if !seen[key] {
				seen[key] = true
				out = append(out, s)
			}
		}
	}
	return out
}
