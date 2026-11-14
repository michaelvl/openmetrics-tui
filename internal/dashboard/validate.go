package dashboard

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type validator struct {
	errs  []Issue
	warns []Issue
}

func (v *validator) errf(line int, format string, a ...any) {
	v.errs = append(v.errs, Issue{Line: line, Msg: fmt.Sprintf(format, a...)})
}

func (v *validator) warnf(line int, format string, a ...any) {
	v.warns = append(v.warns, Issue{Line: line, Msg: fmt.Sprintf(format, a...)})
}

func (v *validator) dashboard(top *yaml.Node, raw *rawDashboard) *Dashboard {
	d := &Dashboard{Title: raw.Title, Datasource: Datasource{URL: raw.Datasource.URL}}

	if strings.TrimSpace(raw.Title) == "" {
		v.errf(lineOf(top, "title"), "title is required")
	}
	if u := raw.Datasource.URL; u != "" {
		_, dsNode := mapGet(top, "datasource")
		if pu, err := url.Parse(u); err != nil || pu.Scheme == "" || pu.Host == "" {
			v.errf(lineOf(dsNode, "url"), "datasource.url %q is not a valid URL", u)
		}
	}

	d.Refresh = DefaultRefresh
	if raw.Refresh != "" {
		if dur, err := parseDuration(raw.Refresh); err != nil || dur < time.Second {
			v.errf(lineOf(top, "refresh"), "refresh %q must be a duration of at least 1s, for example 10s", raw.Refresh)
		} else {
			d.Refresh = dur
		}
	}

	d.Spark = DefaultSpark
	if raw.Spark != "" {
		if dur, err := parseDuration(raw.Spark); err != nil || dur <= 0 {
			v.errf(lineOf(top, "spark"), "spark %q must be a positive duration, for example 1h", raw.Spark)
		} else {
			d.Spark = dur
		}
	}

	d.Windows = v.windows(top, raw.Windows)
	d.Variables = v.variables(top, &raw.Variables)
	d.Scrape = v.scrape(top, raw.Scrape)
	d.Panels = v.panels(top, d, raw.Panels)
	return d
}

func (v *validator) windows(top *yaml.Node, ws []string) []Window {
	if _, n := mapGet(top, "windows"); n == nil {
		return append([]Window(nil), DefaultWindows...)
	}
	line := lineOf(top, "windows")
	if len(ws) == 0 {
		v.errf(line, "windows must list at least \"now\"")
		return nil
	}
	if ws[0] != "now" {
		v.errf(line, "windows must start with \"now\", got %q", ws[0])
	}
	out := []Window{{Label: "now"}}
	var prev time.Duration
	for _, s := range ws[1:] {
		dur, err := parseDuration(s)
		switch {
		case s == "now":
			v.errf(line, "window \"now\" may only appear first")
		case err != nil || dur <= 0:
			v.errf(line, "invalid window %q: want a positive duration such as 5m or 1h", s)
		case dur <= prev:
			v.errf(line, "window %q must be larger than the one before it", s)
		default:
			prev = dur
			out = append(out, Window{Label: s, Offset: dur})
		}
	}
	return out
}

var (
	validUnits  = map[string]bool{"": true, "none": true, "percent": true, "s": true, "/min": true}
	validAggs   = map[string]bool{"": true, "avg": true, "max": true, "min": true}
	validSorts  = map[string]bool{"": true, "asc": true, "desc": true}
	labelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

func (v *validator) panels(top *yaml.Node, d *Dashboard, raws []rawPanel) []Panel {
	_, seq := mapGet(top, "panels")
	if len(raws) == 0 {
		v.errf(lineOf(top, "panels"), "panels is required and must list at least one panel")
		return nil
	}
	vals := map[string]string{}
	known := map[string]bool{}
	for _, vr := range d.Variables {
		vals[vr.Name] = vr.Default
		known[vr.Name] = true
	}
	used := map[string]bool{}
	titles := map[string]int{}
	var out []Panel
	for i, r := range raws {
		var pn *yaml.Node
		if seq != nil && i < len(seq.Content) {
			pn = seq.Content[i]
		}
		line := 0
		if pn != nil {
			line = pn.Line
		}
		p := Panel{
			Title: r.Title, Query: r.Query, Label: r.Label, Unit: r.Unit,
			Sort: r.Sort, Agg: r.Agg, Folded: r.Folded, Line: line,
		}
		if p.Unit == "none" {
			p.Unit = ""
		}

		if strings.TrimSpace(r.Title) == "" {
			v.errf(line, "panel %d: title is required", i+1)
		} else if first, dup := titles[r.Title]; dup {
			v.errf(lineOf(pn, "title"), "duplicate panel title %q (first used at line %d)", r.Title, first)
		} else {
			titles[r.Title] = lineOf(pn, "title")
		}
		name := fmt.Sprintf("panel %q", r.Title)
		if r.Title == "" {
			name = fmt.Sprintf("panel %d", i+1)
		}

		qline := lineOf(pn, "query")
		if strings.TrimSpace(r.Query) == "" {
			v.errf(qline, "%s: query is required", name)
		} else {
			for _, ref := range Refs(r.Query) {
				used[ref] = true
				if !known[ref] {
					v.errf(qline, "%s: query references undefined variable ${%s}", name, ref)
				}
			}
			q, err := Substitute(r.Query, vals)
			if err == nil {
				if err := Validate(q); err != nil {
					v.errf(qline, "%s: invalid query: %v", name, err)
				}
			}
		}

		if !validUnits[r.Unit] {
			v.errf(lineOf(pn, "unit"), "%s: unknown unit %q (want percent, s, /min or none)", name, r.Unit)
		}
		if !validAggs[r.Agg] {
			v.errf(lineOf(pn, "agg"), "%s: unknown agg %q (want avg, max or min)", name, r.Agg)
		}
		if !validSorts[r.Sort] {
			v.errf(lineOf(pn, "sort"), "%s: unknown sort %q (want asc or desc)", name, r.Sort)
		}
		if r.Limit != nil {
			if *r.Limit <= 0 {
				v.errf(lineOf(pn, "limit"), "%s: limit must be a positive integer", name)
			} else {
				p.Limit = *r.Limit
			}
		}
		if msg := checkLabelTemplate(r.Label); msg != "" {
			v.errf(lineOf(pn, "label"), "%s: label template: %s", name, msg)
		}
		p.Thresholds = v.thresholds(pn, name, r.Thresholds)
		out = append(out, p)
	}
	for _, vr := range d.Variables {
		if !used[vr.Name] {
			v.warnf(vr.Line, "variable %q is not used by any panel query", vr.Name)
		}
	}
	return out
}

func (v *validator) thresholds(pn *yaml.Node, name string, raws []rawThreshold) []Threshold {
	_, seq := mapGet(pn, "thresholds")
	var out []Threshold
	for i, r := range raws {
		line := lineOf(pn, "thresholds")
		if seq != nil && i < len(seq.Content) {
			line = seq.Content[i].Line
		}
		var ops []Threshold
		for _, c := range []struct {
			op string
			v  *float64
		}{{"gt", r.Gt}, {"gte", r.Gte}, {"lt", r.Lt}, {"lte", r.Lte}} {
			if c.v != nil {
				ops = append(ops, Threshold{Op: c.op, Value: *c.v})
			}
		}
		t := Threshold{Color: r.Color}
		switch {
		case len(ops) > 1:
			v.errf(line, "%s: threshold %d has more than one comparison (use one of gt, gte, lt, lte)", name, i+1)
		case len(ops) == 1:
			t = ops[0]
			t.Color = r.Color
		}
		if strings.TrimSpace(r.Color) == "" {
			v.errf(line, "%s: threshold %d needs a color", name, i+1)
		}
		if t.Op == "" && len(ops) == 0 && i != len(raws)-1 {
			v.errf(line, "%s: catch-all threshold %d must be last; later entries can never match", name, i+1)
		}
		out = append(out, t)
	}
	return out
}

// checkLabelTemplate returns a description of the first syntax error in a
// label template, or "". Only {{labelname}} substitutions are supported.
func checkLabelTemplate(t string) string {
	for rest := t; ; {
		open := strings.Index(rest, "{{")
		closeIdx := strings.Index(rest, "}}")
		if open < 0 {
			if closeIdx >= 0 {
				return `unmatched "}}"`
			}
			return ""
		}
		if closeIdx >= 0 && closeIdx < open {
			return `unmatched "}}"`
		}
		end := strings.Index(rest[open:], "}}")
		if end < 0 {
			return `unterminated "{{"`
		}
		name := strings.TrimSpace(rest[open+2 : open+end])
		if !labelNameRe.MatchString(name) {
			return fmt.Sprintf("%q is not a label name; only {{labelname}} is supported", name)
		}
		rest = rest[open+end+2:]
	}
}

func (v *validator) scrape(top *yaml.Node, r *rawScrape) *Scrape {
	if r == nil {
		return nil
	}
	_, sn := mapGet(top, "scrape")
	s := &Scrape{
		Namespaces: r.Namespaces, Selector: r.Selector,
		Annotation: r.Annotation, Port: r.Port, Path: r.Path,
	}
	if len(r.Namespaces) == 0 {
		v.errf(lineOf(sn, "namespaces"), "scrape.namespaces is required and must list at least one namespace")
	}
	for _, ns := range r.Namespaces {
		if ns == "" {
			v.errf(lineOf(sn, "namespaces"), "scrape.namespaces contains an empty name")
		}
	}
	s.Interval = 15 * time.Second
	if r.Interval != "" {
		if dur, err := parseDuration(r.Interval); err != nil || dur < time.Second {
			v.errf(lineOf(sn, "interval"), "scrape.interval %q must be a duration of at least 1s", r.Interval)
		} else {
			s.Interval = dur
		}
	}
	if r.Selector != "" && r.Annotation != "" {
		v.errf(lineOf(sn, "selector"), "scrape.selector and scrape.annotation are mutually exclusive")
	}
	if r.Annotation == "" {
		// Selector (or scrape-everything) mode: port is a number, path a literal.
		if r.Port != "" {
			var n int
			if _, err := fmt.Sscanf(r.Port, "%d", &n); err != nil || fmt.Sprint(n) != r.Port || n < 1 || n > 65535 {
				v.errf(lineOf(sn, "port"), "scrape.port %q must be a port number between 1 and 65535", r.Port)
			}
		}
		if r.Path != "" && !strings.HasPrefix(r.Path, "/") {
			v.errf(lineOf(sn, "path"), "scrape.path %q must start with /", r.Path)
		}
	}
	return s
}
