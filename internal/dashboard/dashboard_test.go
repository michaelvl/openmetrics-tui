package dashboard

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestValidFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/valid/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no valid fixtures: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if _, err := Load(f); err != nil {
				t.Fatalf("expected valid, got:\n%v", err)
			}
		})
	}
}

func TestInvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/invalid/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no invalid fixtures: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(data)
			var ve *Error
			if !errors.As(err, &ve) {
				t.Fatalf("expected *Error, got %v", err)
			}
			got := ve.Error() + "\n"
			golden := strings.TrimSuffix(f, ".yaml") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("errors differ\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestLoadPrefixesFileName(t *testing.T) {
	_, err := Load("testdata/invalid/duplicate-titles.yaml")
	if err == nil || !strings.HasPrefix(err.Error(), "testdata/invalid/duplicate-titles.yaml:5: ") {
		t.Fatalf("got %v", err)
	}
}

func TestDefaultsAndFull(t *testing.T) {
	d, err := Load("testdata/valid/minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if d.Refresh != DefaultRefresh || d.Spark != DefaultSpark || len(d.Windows) != 4 {
		t.Errorf("defaults not applied: %+v", d)
	}

	d, err = Load("testdata/valid/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if d.Refresh != 5*time.Second || d.Spark != 30*time.Minute {
		t.Errorf("refresh/spark: %v %v", d.Refresh, d.Spark)
	}
	if got := d.Windows[3]; got.Label != "1d" || got.Offset != 24*time.Hour {
		t.Errorf("window: %+v", got)
	}
	if len(d.Variables) != 3 || d.Variables[0].Mode != OptionsList || d.Variables[1].Mode != OptionsNone ||
		d.Variables[2].Mode != OptionsLabelValues || d.Variables[2].Label != "service" {
		t.Errorf("variables: %+v", d.Variables)
	}
	p := d.Panels[0]
	if p.Agg != "max" || p.Sort != "asc" || p.Limit != 3 || !p.Folded || p.Unit != "" {
		t.Errorf("panel: %+v", p)
	}
	want := []Threshold{{"gte", 4, "red"}, {"lt", 1, "green"}, {"", 0, "yellow"}}
	for i, th := range p.Thresholds {
		if th != want[i] {
			t.Errorf("threshold %d: %+v", i, th)
		}
	}
	if d.Scrape == nil || d.Scrape.Interval != 15*time.Second || d.Scrape.Port != "8080" {
		t.Errorf("scrape: %+v", d.Scrape)
	}
}

func TestUnusedVariableWarning(t *testing.T) {
	d, err := Load("testdata/valid/unused-variable.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings) != 1 || d.Warnings[0].Line != 5 || !strings.Contains(d.Warnings[0].Msg, `"extra"`) {
		t.Errorf("warnings: %+v", d.Warnings)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(`sum by (a) (rate(foo{a="x"}[5m])) * 60`); err != nil {
		t.Errorf("valid query rejected: %v", err)
	}
	if err := Validate(`sum(`); err == nil {
		t.Error("broken query accepted")
	}
}

func TestSubstituteEscapes(t *testing.T) {
	got, err := Substitute(`up{a="${x}"}`, map[string]string{"x": `a"b\c'd`})
	if err != nil {
		t.Fatal(err)
	}
	if want := `up{a="a\"b\\c\x27d"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if err := Validate(got); err != nil {
		t.Errorf("escaped query does not parse: %v", err)
	}
	if _, err := Substitute(`up{a="${y}"}`, nil); err == nil {
		t.Error("undefined variable accepted")
	}
}

func TestValuesOverrides(t *testing.T) {
	d, err := Load("testdata/valid/appendix-a.yaml")
	if err != nil {
		t.Fatal(err)
	}
	vals, err := d.Values(map[string]string{"namespace": "staging"})
	if err != nil || vals["namespace"] != "staging" || vals["controller"] != ".*" {
		t.Errorf("vals %v, err %v", vals, err)
	}
	for _, bad := range []map[string]string{
		{"nope": "x"},
		{"namespace": `x" or "1"="1`}, // fails the pattern
		{"namespace": "UPPER"},
		{"controller": "("}, // not a regex
	} {
		if _, err := d.Values(bad); err == nil {
			t.Errorf("override %v accepted", bad)
		}
	}
}

func TestDefaultPatternRejectsMetacharacters(t *testing.T) {
	d, err := Parse([]byte("title: t\nvariables:\n  v: {default: a}\npanels:\n  - {title: p, query: 'up{a=\"${v}\"}'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"a.*", `a"b`, "a b", "a|b"} {
		if err := d.Variables[0].Check(s); err == nil {
			t.Errorf("%q accepted by default pattern", s)
		}
	}
	if err := d.Variables[0].Check("ns-1.x:80/y"); err != nil {
		t.Error(err)
	}
}
