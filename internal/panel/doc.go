// Package panel holds the pure logic behind a dashboard panel: turning a
// range query response into rows and window columns, thresholds, labels,
// unit formatting, sorting, folded summaries and sparklines. It has no
// terminal or network code, so everything is testable directly. It lives
// apart from internal/dashboard because it works on query results
// (promapi.Series), while dashboard only handles the definition.
package panel
