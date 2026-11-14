package dashboard

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPattern is the value pattern for variables that do not set one. It
// only allows characters that are inert in a PromQL string and a regex.
const DefaultPattern = `^[A-Za-z0-9_.:/-]*$`

// OptionsMode says where suggestions for a variable come from.
type OptionsMode int

const (
	// OptionsDerived: derive the lookup from the panel queries (Phase 3).
	OptionsDerived OptionsMode = iota
	// OptionsList: Values is the explicit list.
	OptionsList
	// OptionsLabelValues: look up Label values of Metric.
	OptionsLabelValues
	// OptionsNone: no suggestions, free text.
	OptionsNone
)

// Variable is a named, user-editable query parameter.
type Variable struct {
	Name    string
	Default string
	Mode    OptionsMode
	Values  []string // OptionsList
	Metric  string   // OptionsLabelValues
	Label   string   // OptionsLabelValues
	Regex   bool
	// Pattern validates values before substitution. With Regex and no
	// explicit pattern it is nil and the value must compile as a regex.
	Pattern *regexp.Regexp
	Line    int
}

var (
	varNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	refRe     = regexp.MustCompile(`\$\{([^}]*)\}`)
)

type rawVariable struct {
	Default string    `yaml:"default"`
	Options yaml.Node `yaml:"options"`
	Pattern string    `yaml:"pattern"`
	Regex   bool      `yaml:"regex"`
}

var variableKeys = map[string]bool{"default": true, "options": true, "pattern": true, "regex": true}

func (v *validator) variables(top, n *yaml.Node) []Variable {
	if n.Kind == 0 {
		return nil
	}
	line := lineOf(top, "variables")
	if n.Kind != yaml.MappingNode {
		v.errf(line, "variables must be a mapping of name to definition")
		return nil
	}
	if len(n.Content)/2 > MaxVariables {
		v.errf(line, "at most %d variables are supported, found %d", MaxVariables, len(n.Content)/2)
	}
	var out []Variable
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn, vn := n.Content[i], n.Content[i+1]
		vr := Variable{Name: kn.Value, Line: kn.Line}
		if !varNameRe.MatchString(kn.Value) {
			v.errf(kn.Line, "variable name %q must match [A-Za-z_][A-Za-z0-9_]*", kn.Value)
		}
		if seen[kn.Value] {
			v.errf(kn.Line, "duplicate variable %q", kn.Value)
		}
		seen[kn.Value] = true
		if vn.Kind != yaml.MappingNode {
			v.errf(kn.Line, "variable %q must be a mapping with a default", kn.Value)
			continue
		}
		// Node.Decode does not honour KnownFields, so check keys by hand.
		for j := 0; j+1 < len(vn.Content); j += 2 {
			if k := vn.Content[j]; !variableKeys[k.Value] {
				v.errf(k.Line, "unknown key %q", k.Value)
			}
		}
		var raw rawVariable
		_ = vn.Decode(&raw) // type problems are reported by the strict decode
		vr.Default, vr.Regex = raw.Default, raw.Regex
		if k, _ := mapGet(vn, "default"); k == nil {
			v.errf(kn.Line, "variable %q: default is required", kn.Value)
		}

		switch {
		case raw.Pattern != "":
			re, err := regexp.Compile(raw.Pattern)
			if err != nil {
				v.errf(lineOf(vn, "pattern"), "variable %q: invalid pattern: %v", kn.Value, err)
			} else {
				vr.Pattern = re
			}
		case !raw.Regex:
			vr.Pattern = regexp.MustCompile(DefaultPattern)
		}
		v.options(&vr, vn, &raw)

		if k, _ := mapGet(vn, "default"); k != nil {
			if err := vr.Check(vr.Default); err != nil {
				v.errf(lineOf(vn, "default"), "variable %q: default: %v", kn.Value, err)
			}
		}
		out = append(out, vr)
	}
	return out
}

func (v *validator) options(vr *Variable, vn *yaml.Node, raw *rawVariable) {
	on := &raw.Options
	line := lineOf(vn, "options")
	switch on.Kind {
	case 0:
	case yaml.SequenceNode:
		vr.Mode = OptionsList
		if len(on.Content) == 0 {
			v.errf(line, "variable %q: options list is empty", vr.Name)
		}
		for _, it := range on.Content {
			if it.Kind != yaml.ScalarNode {
				v.errf(it.Line, "variable %q: options must be strings", vr.Name)
				continue
			}
			vr.Values = append(vr.Values, it.Value)
			if err := vr.Check(it.Value); err != nil {
				v.errf(it.Line, "variable %q: option: %v", vr.Name, err)
			}
		}
	case yaml.ScalarNode:
		if on.Value == "false" && on.Tag == "!!bool" {
			vr.Mode = OptionsNone
		} else {
			v.errf(line, "variable %q: options must be a list, false, or {label_values: {metric, label}}", vr.Name)
		}
	case yaml.MappingNode:
		_, lv := mapGet(on, "label_values")
		if lv == nil || len(on.Content) != 2 || lv.Kind != yaml.MappingNode {
			v.errf(line, "variable %q: options mapping must be exactly {label_values: {metric, label}}", vr.Name)
			return
		}
		for j := 0; j+1 < len(lv.Content); j += 2 {
			if k := lv.Content[j]; k.Value != "metric" && k.Value != "label" {
				v.errf(k.Line, "unknown key %q", k.Value)
			}
		}
		_, m := mapGet(lv, "metric")
		_, l := mapGet(lv, "label")
		if m == nil || m.Value == "" || l == nil || l.Value == "" {
			v.errf(lv.Line, "variable %q: label_values needs both metric and label", vr.Name)
			return
		}
		if !labelNameRe.MatchString(l.Value) {
			v.errf(l.Line, "variable %q: %q is not a label name", vr.Name, l.Value)
		}
		vr.Mode, vr.Metric, vr.Label = OptionsLabelValues, m.Value, l.Value
	default:
		v.errf(line, "variable %q: invalid options", vr.Name)
	}
}

// Check reports whether val is acceptable for the variable: it must match the
// pattern, compile as a regex when Regex is set, and contain nothing that
// could not be safely quoted in a PromQL string.
func (vr *Variable) Check(val string) error {
	if strings.ContainsAny(val, "`\x00\n\r") {
		return errors.New("value contains a backtick or control character")
	}
	if vr.Pattern != nil && !vr.Pattern.MatchString(val) {
		return fmt.Errorf("value %q does not match pattern %s", val, vr.Pattern)
	}
	if vr.Regex {
		if _, err := regexp.Compile(val); err != nil {
			return fmt.Errorf("value %q is not a valid regex: %v", val, err)
		}
	}
	return nil
}

// Values merges the variable defaults with overrides (for example from
// --var), checking each override. Unknown names are an error.
func (d *Dashboard) Values(overrides map[string]string) (map[string]string, error) {
	vals := make(map[string]string, len(d.Variables))
	for _, vr := range d.Variables {
		vals[vr.Name] = vr.Default
	}
	names := make([]string, 0, len(overrides))
	for n := range overrides {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		var found *Variable
		for i := range d.Variables {
			if d.Variables[i].Name == n {
				found = &d.Variables[i]
			}
		}
		if found == nil {
			return nil, fmt.Errorf("unknown variable %q", n)
		}
		if err := found.Check(overrides[n]); err != nil {
			return nil, fmt.Errorf("variable %q: %w", n, err)
		}
		vals[n] = overrides[n]
	}
	return vals, nil
}

// Refs returns the variable names referenced as ${name} in a query.
func Refs(query string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(query, -1) {
		out = append(out, m[1])
	}
	return out
}

// Substitute replaces each ${name} in query with its value, escaped for use
// inside a PromQL string literal, so a value can never end the matcher.
func Substitute(query string, vals map[string]string) (string, error) {
	var missing []string
	out := refRe.ReplaceAllStringFunc(query, func(m string) string {
		name := m[2 : len(m)-1]
		val, ok := vals[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return escapeString(val)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("undefined variable ${%s}", missing[0])
	}
	return out, nil
}

var stringEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `'`, `\x27`)

func escapeString(s string) string { return stringEscaper.Replace(s) }
