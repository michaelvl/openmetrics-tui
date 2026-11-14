package dashboard

import (
	"github.com/prometheus/prometheus/promql/parser"
)

// Validate checks the PromQL syntax of query offline, without a backend. It
// is the only place the PromQL parser is used. Variables must already be
// substituted.
func Validate(query string) error {
	_, err := parser.ParseExpr(query)
	return err
}
