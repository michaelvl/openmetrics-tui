package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitFilterClausesLeavesRegexCommasAlone(t *testing.T) {
	tests := []struct {
		name   string
		filter string
		want   []string
	}{
		{"empty filter has no clauses", "", nil},
		{"a single clause", "env=prod", []string{"env=prod"}},
		{"two clauses", "env=prod,region=eu", []string{"env=prod", "region=eu"}},
		{"spaces around a separator are not part of the clause", "env=prod, region=eu",
			[]string{"env=prod", "region=eu"}},
		{"a repetition is one clause", "env=~prod{1,2}", []string{"env=~prod{1,2}"}},
		{"a character class is one clause", "zone=~[a-z,]+", []string{"zone=~[a-z,]+"}},
		{"a class closing bracket in first position is a literal", "zone=~[],]+x,env=prod",
			[]string{"zone=~[],]+x", "env=prod"}},
		{"a nested repetition still closes", "a=~(x{1,2}){3,4},b=y",
			[]string{"a=~(x{1,2}){3,4}", "b=y"}},
		{"an escaped comma is data", `env=a\,b`, []string{"env=a,b"}},
		{"other escapes pass through whole", `env=~\d+,b=y`, []string{`env=~\d+`, "b=y"}},
		{"a trailing comma leaves an empty clause", "env=prod,", []string{"env=prod", ""}},
		{"a doubled comma leaves an empty clause", "env=prod,,b=y", []string{"env=prod", "", "b=y"}},
	}
	for _, tc := range tests {
		if got := splitFilterClauses(tc.filter); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: splitFilterClauses(%q) = %q, want %q", tc.name, tc.filter, got, tc.want)
		}
	}
}

func TestParseLabelClauseTakesTheFirstOperator(t *testing.T) {
	tests := []struct {
		clause    string
		wantKey   string
		wantOp    clauseOp
		wantValue string
	}{
		{"env=prod", "env", opEq, "prod"},
		{"env!=dev", "env", opNe, "dev"},
		{"env=~pro.*", "env", opRe, "pro.*"},
		{"env!~dev|test", "env", opNre, "dev|test"},
		{"url=~http://x?a=1", "url", opRe, "http://x?a=1"},
		{"env=", "env", opEq, ""},
		{"=prod", "", opEq, "prod"},
		{"pro.*", "", opNone, "pro.*"},
		{"a!b", "", opNone, "a!b"},  // a lone ! is not an operator
		{"a!b=c", "a!b", opEq, "c"}, // ... so the = after it still is
		{"trailing!", "", opNone, "trailing!"},
	}
	for _, tc := range tests {
		key, op, value := parseLabelClause(tc.clause)
		if key != tc.wantKey || op != tc.wantOp || value != tc.wantValue {
			t.Errorf("parseLabelClause(%q) = (%q, %d, %q), want (%q, %d, %q)",
				tc.clause, key, op, value, tc.wantKey, tc.wantOp, tc.wantValue)
		}
	}
}

func TestMatchesLabelFilterAndsItsClauses(t *testing.T) {
	labels := map[string]string{"env": "prod", "region": "eu", "handler": "/api"}

	tests := []struct {
		name   string
		filter string
		want   bool
	}{
		{"an empty filter admits everything", "", true},
		{"both clauses hold", "env=prod,region=eu", true},
		{"one clause failing rejects the series", "env=prod,region=us", false},
		{"three clauses", "env=prod,region=eu,handler=~^/api", true},
		{"negation holds", "env=prod,region!=us", true},
		{"negation rejects", "env=prod,region!=eu", false},
		{"negated regex holds", "env!~dev|test", true},
		{"negated regex rejects", "env!~pro.*", false},
		{"a bare regex mixes with a keyed clause", "env=prod,^/api$", true},
		{"a bare regex is still an or across labels", "^/api$", true},
		{"a bare regex matching nothing rejects", "^/nope$", false},
		{"a repetition survives the split", "env=~pro{1,2}d", true},
		{"a class survives the split", "region=~[a-z,]+", true},
	}
	for _, tc := range tests {
		if got := matchesLabelFilter(tc.filter, labels); got != tc.want {
			t.Errorf("%s: matchesLabelFilter(%q) = %v, want %v", tc.name, tc.filter, got, tc.want)
		}
	}
}

// A label a series does not carry reads as empty, the way PromQL treats it,
// which is what makes a negated clause mean what people expect it to.
func TestAbsentLabelsReadAsEmpty(t *testing.T) {
	noEnv := map[string]string{"region": "eu"}

	tests := []struct {
		name   string
		filter string
		want   bool
	}{
		{"a negated clause admits a series without the label", "env!=dev", true},
		{"a negated regex admits it too", "env!~dev|test", true},
		{"a positive clause still rejects it", "env=prod", false},
		{"an empty value asks for the label to be absent", "env=", true},
		{"... and rejects a series that has one", "region=", false},
		{"a regex matching empty admits it", "env=~.*", true},
		{"a regex not matching empty rejects it", "env=~.+", false},
		{"absent plus present", "env!=dev,region=eu", true},
	}
	for _, tc := range tests {
		if got := matchesLabelFilter(tc.filter, noEnv); got != tc.want {
			t.Errorf("%s: matchesLabelFilter(%q) = %v, want %v", tc.name, tc.filter, got, tc.want)
		}
	}
}

func TestFilteredLabelKeysAreOnlyTheOnesPinnedDown(t *testing.T) {
	tests := []struct {
		name   string
		filter string
		want   []string
	}{
		{"nothing filtered", "", []string{}},
		{"one key", "env=prod", []string{"env"}},
		{"every positive clause contributes", "env=prod,region=~eu.*", []string{"env", "region"}},
		{"a negated clause does not pin a value down", "env=prod,region!=us", []string{"env"}},
		{"nor does a negated regex", "env!~dev", []string{}},
		{"a bare regex names no label", "prod", []string{}},
		{"mixed", "prod,env=prod,region!=us,zone=eu", []string{"env", "zone"}},
	}
	for _, tc := range tests {
		if got := getFilteredLabelKeys(tc.filter); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: getFilteredLabelKeys(%q) = %q, want %q", tc.name, tc.filter, got, tc.want)
		}
	}
}

// With several clauses the footer cannot show the whole filter, so the
// complaint has to say which clause it is about.
func TestAClauseErrorNamesItsClause(t *testing.T) {
	err := validateLabelFilter("env=prod,region=~eu(")
	if err == nil {
		t.Fatal("validateLabelFilter accepted a broken clause")
	}
	if !strings.HasPrefix(err.Error(), "region: ") {
		t.Errorf("error = %q, want it to name the region clause", err)
	}

	// An empty clause has no name to give, and "" + ": " in front of the
	// complaint is noise.
	err = validateLabelFilter("env=prod,")
	if err == nil {
		t.Fatal("validateLabelFilter accepted a trailing comma")
	}
	if err.Error() != "empty filter clause" {
		t.Errorf("error = %q, want the bare complaint", err)
	}

	// A single clause is already visible in the box above the footer, so naming
	// it would only spend room the complaint itself needs.
	err = validateLabelFilter("region=~eu(")
	if err == nil {
		t.Fatal("validateLabelFilter accepted a broken clause")
	}
	if strings.Contains(err.Error(), "region:") {
		t.Errorf("error = %q, want the bare complaint for a single clause", err)
	}
}
