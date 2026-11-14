// Package dashboard defines the YAML dashboard format used by omtui-dash:
// loading, strict validation with line-numbered errors, and variable
// substitution. Queries are only parsed here, never evaluated.
package dashboard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"gopkg.in/yaml.v3"
)

// Defaults applied when the YAML leaves a setting out.
const (
	DefaultRefresh = 10 * time.Second
	DefaultSpark   = time.Hour
	MaxVariables   = 9
)

// DefaultWindows are the columns used when `windows` is omitted.
var DefaultWindows = []Window{
	{Label: "now"},
	{Label: "5m", Offset: 5 * time.Minute},
	{Label: "15m", Offset: 15 * time.Minute},
	{Label: "1h", Offset: time.Hour},
}

// Dashboard is a validated dashboard definition.
type Dashboard struct {
	Title      string
	Datasource Datasource
	Refresh    time.Duration
	Windows    []Window
	Spark      time.Duration
	// Variables are in declaration order; index i is bound to key i+1.
	Variables []Variable
	// Scrape is nil when the YAML has no scrape section.
	Scrape *Scrape
	Panels []Panel
	// Warnings are non-fatal findings, such as unused variables.
	Warnings []Issue
}

// Datasource names the backend; --prom overrides URL.
type Datasource struct {
	URL string
}

// Window is one column. Offset 0 is the "now" column.
type Window struct {
	Label  string
	Offset time.Duration
}

// Panel is one query rendered as a table of series.
type Panel struct {
	Title      string
	Query      string // as written, with ${name} placeholders
	Label      string // row title template, {{labelname}} substitutions only
	Unit       string // "", "percent", "s" or "/min"
	Thresholds []Threshold
	Sort       string // "", "asc" or "desc"
	Limit      int    // 0 means no limit
	Agg        string // "", "avg", "max" or "min"
	Folded     bool
	Line       int // line of the panel in the YAML
}

// Threshold is one ordered rule; the first match wins.
type Threshold struct {
	Op     string // "gt", "gte", "lt", "lte", or "" for the catch-all
	Value  float64
	Color  string
	Symbol string // marker drawn with the colour; "" for the colour's default
}

// Marker is the status symbol of the rule. The default has a distinct shape per
// well-known colour, so the status reads without colour.
func (t Threshold) Marker() string {
	if t.Symbol != "" {
		return t.Symbol
	}
	switch t.Color {
	case "red":
		return "▲"
	case "orange":
		return "◆"
	case "yellow":
		return "■"
	}
	return "●"
}

// Scrape configures managed mode scraping (Phase 4); it is only validated here.
type Scrape struct {
	Namespaces []string
	Interval   time.Duration
	Selector   string
	Annotation string
	// Port is an annotation name in annotation mode and a number otherwise.
	Port string
	Path string
}

// Issue is a finding tied to a line of the YAML (0 when unknown).
type Issue struct {
	Line int
	Msg  string
}

func (i Issue) String() string {
	if i.Line > 0 {
		return fmt.Sprintf("line %d: %s", i.Line, i.Msg)
	}
	return i.Msg
}

// Error is the set of validation errors found in one dashboard file.
type Error struct {
	File   string
	Issues []Issue
}

func (e *Error) Error() string {
	var b strings.Builder
	for n, i := range e.Issues {
		if n > 0 {
			b.WriteByte('\n')
		}
		if e.File != "" {
			if i.Line > 0 {
				fmt.Fprintf(&b, "%s:%d: %s", e.File, i.Line, i.Msg)
			} else {
				fmt.Fprintf(&b, "%s: %s", e.File, i.Msg)
			}
		} else {
			b.WriteString(i.String())
		}
	}
	return b.String()
}

// Load reads and validates a dashboard file. On validation failure the error
// is an *Error listing every problem.
func Load(path string) (*Dashboard, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d, err := Parse(data)
	var ve *Error
	if errors.As(err, &ve) {
		ve.File = path
	}
	return d, err
}

// rawDashboard mirrors the YAML. Fields that need line numbers or order are
// kept as nodes and handled by hand.
type rawDashboard struct {
	Title      string `yaml:"title"`
	Datasource struct {
		URL string `yaml:"url"`
	} `yaml:"datasource"`
	Refresh   string     `yaml:"refresh"`
	Windows   []string   `yaml:"windows"`
	Spark     string     `yaml:"spark"`
	Variables yaml.Node  `yaml:"variables"`
	Scrape    *rawScrape `yaml:"scrape"`
	Panels    []rawPanel `yaml:"panels"`
}

type rawScrape struct {
	Namespaces []string `yaml:"namespaces"`
	Interval   string   `yaml:"interval"`
	Selector   string   `yaml:"selector"`
	Annotation string   `yaml:"annotation"`
	Port       string   `yaml:"port"`
	Path       string   `yaml:"path"`
}

type rawPanel struct {
	Title      string         `yaml:"title"`
	Query      string         `yaml:"query"`
	Label      string         `yaml:"label"`
	Unit       string         `yaml:"unit"`
	Thresholds []rawThreshold `yaml:"thresholds"`
	Sort       string         `yaml:"sort"`
	Limit      *int           `yaml:"limit"`
	Agg        string         `yaml:"agg"`
	Folded     bool           `yaml:"folded"`
}

type rawThreshold struct {
	Gt     *float64 `yaml:"gt"`
	Gte    *float64 `yaml:"gte"`
	Lt     *float64 `yaml:"lt"`
	Lte    *float64 `yaml:"lte"`
	Color  string   `yaml:"color"`
	Symbol string   `yaml:"symbol"`
}

var (
	lineRe    = regexp.MustCompile(`line (\d+)`)
	typeErrRe = regexp.MustCompile(`^line (\d+): (.*)$`)
	notFound  = regexp.MustCompile(`^field (\S+) not found in type `)
)

// Parse decodes and validates dashboard YAML. Unknown keys are errors. On
// failure the returned error is an *Error with line-numbered issues.
func Parse(data []byte) (*Dashboard, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, &Error{Issues: []Issue{syntaxIssue(err)}}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, &Error{Issues: []Issue{{Line: 1, Msg: "dashboard must be a YAML mapping"}}}
	}
	top := root.Content[0]

	v := &validator{}
	var raw rawDashboard
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			for _, m := range te.Errors {
				v.errs = append(v.errs, typeIssue(m))
			}
		} else {
			v.errs = append(v.errs, syntaxIssue(err))
		}
	}

	d := v.dashboard(top, &raw)
	if len(v.errs) > 0 {
		sort.SliceStable(v.errs, func(a, b int) bool { return v.errs[a].Line < v.errs[b].Line })
		return nil, &Error{Issues: v.errs}
	}
	sort.SliceStable(v.warns, func(a, b int) bool { return v.warns[a].Line < v.warns[b].Line })
	d.Warnings = v.warns
	return d, nil
}

func syntaxIssue(err error) Issue {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	line := 0
	if m := lineRe.FindStringSubmatch(msg); m != nil {
		line, _ = strconv.Atoi(m[1])
		msg = strings.Replace(msg, "line "+m[1]+": ", "", 1)
	}
	return Issue{Line: line, Msg: msg}
}

func typeIssue(m string) Issue {
	sm := typeErrRe.FindStringSubmatch(m)
	if sm == nil {
		return Issue{Msg: m}
	}
	line, _ := strconv.Atoi(sm[1])
	msg := sm[2]
	if nf := notFound.FindStringSubmatch(msg); nf != nil {
		msg = fmt.Sprintf("unknown key %q", nf[1])
	}
	return Issue{Line: line, Msg: msg}
}

// mapGet returns the key and value nodes for key in a mapping node.
func mapGet(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// lineOf is the line of key's value in m, falling back to the mapping itself.
func lineOf(m *yaml.Node, key string) int {
	if _, v := mapGet(m, key); v != nil {
		return v.Line
	}
	if m != nil {
		return m.Line
	}
	return 0
}

// parseDuration accepts Prometheus durations such as 30s, 5m, 1h, 1d.
func parseDuration(s string) (time.Duration, error) {
	d, err := model.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return time.Duration(d), nil
}
