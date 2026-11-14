package dashboard

import (
	"reflect"
	"testing"
)

func TestSelectorsSingle(t *testing.T) {
	got := Selectors(`rate(reconciles_total{namespace="${namespace}",job="ctl"}[5m])`)
	want := []Selector{{
		Variable: "namespace", Label: "namespace", Op: "=", Metric: "reconciles_total",
		Matchers: []Matcher{{Label: "job", Op: "=", Value: "ctl"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestSelectorsMultipleAndRegex(t *testing.T) {
	got := Selectors(`sum by (controller) (rate(errs{namespace="${ns}",controller=~"${ctl}"}[5m])) / sum by (controller) (rate(total{namespace="${ns}"}[5m]))`)
	if len(got) != 3 {
		t.Fatalf("want 3 selectors, got %+v", got)
	}
	if got[1].Variable != "ctl" || got[1].Op != "=~" || got[1].Label != "controller" {
		t.Errorf("regex selector: %+v", got[1])
	}
	if got[1].Matchers[0].Value != "${ns}" {
		t.Errorf("other variable not kept: %+v", got[1].Matchers)
	}
	if got[2].Metric != "total" {
		t.Errorf("binary op rhs: %+v", got[2])
	}
}

func TestSelectorsNestedAggregation(t *testing.T) {
	got := Selectors(`topk(5, sum by (pod) (increase(restarts{namespace="${ns}"}[1h])))`)
	if len(got) != 1 || got[0].Variable != "ns" || got[0].Metric != "restarts" {
		t.Fatalf("got %+v", got)
	}
}

func TestSelectorsSameVariableTwice(t *testing.T) {
	got := Selectors(`a{namespace="${ns}"} + b{namespace="${ns}"}`)
	if len(got) != 2 || got[0].Metric != "a" || got[1].Metric != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestSelectorsIgnoresUnusableMatchers(t *testing.T) {
	if got := Selectors(`up{job!="${j}"}`); len(got) != 0 {
		t.Errorf("negative matcher derived: %+v", got)
	}
	if got := Selectors(`up{job=~"${j}-.*"}`); len(got) != 0 {
		t.Errorf("embedded variable derived: %+v", got)
	}
	if got := Selectors(`up{job="x"}`); len(got) != 0 {
		t.Errorf("no variable: %+v", got)
	}
}

func TestSelectorsUnparseable(t *testing.T) {
	for _, q := range []string{
		`rate(x{ns="${ns}"}[${win}])`, // variable in a range duration
		`x{ns="${ns}"`,                // syntax error
		`x > ${n}`,                    // variable in a number position
		``,
	} {
		if got := Selectors(q); got != nil {
			t.Errorf("%q: want nil, got %+v", q, got)
		}
	}
}

func TestSelectorsNameless(t *testing.T) {
	got := Selectors(`{__name__=~"foo.*",ns="${ns}"}`)
	if len(got) != 1 || got[0].Metric != "" || len(got[0].Matchers) != 1 || got[0].Matchers[0].Label != "__name__" {
		t.Fatalf("got %+v", got)
	}
}

func TestLookupCascades(t *testing.T) {
	sels := Selectors(`rate(errs{namespace="${ns}",controller=~"${ctl}"}[5m])`)
	vals := map[string]string{"ns": `pr"od`, "ctl": ".*"}

	label, match, err := sels[1].Lookup(vals)
	if err != nil {
		t.Fatal(err)
	}
	if label != "controller" || match != `errs{namespace="pr\"od"}` {
		t.Errorf("controller lookup: %q %q", label, match)
	}

	label, match, err = sels[0].Lookup(vals)
	if err != nil {
		t.Fatal(err)
	}
	// The edited variable's own matcher is dropped; $ctl is substituted.
	if label != "namespace" || match != `errs{controller=~".*"}` {
		t.Errorf("namespace lookup: %q %q", label, match)
	}
}

func TestLookupEdgeCases(t *testing.T) {
	s := Selectors(`up{job="${j}"}`)[0]
	if _, m, err := s.Lookup(nil); err != nil || m != "up" {
		t.Errorf("metric only: %q %v", m, err)
	}
	s = Selectors(`{job="${j}"}`)[0]
	if _, m, err := s.Lookup(nil); err != nil || m != "" {
		t.Errorf("nothing left: %q %v", m, err)
	}
	s = Selectors(`up{job="${j}",ns="${n}"}`)[0]
	if _, _, err := s.Lookup(map[string]string{}); err == nil {
		t.Error("missing variable value not reported")
	}
}

func TestSelectorsFor(t *testing.T) {
	qs := []string{`a{ns="${ns}"}`, `a{ns="${ns}"}`, `b{ns="${ns}",c="${c}"}`}
	got := SelectorsFor("ns", qs)
	if len(got) != 2 {
		t.Fatalf("want 2 distinct, got %+v", got)
	}
	if len(SelectorsFor("c", qs)) != 1 {
		t.Error("c")
	}
}
