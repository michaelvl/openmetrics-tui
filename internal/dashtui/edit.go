package dashtui

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
)

type editKind int

const (
	editNone   editKind = iota
	editVar             // a variable box owns the input
	editFilter          // the row filter owns the input
)

// sugState is where a variable's suggestion list stands.
type sugState int

const (
	sugNone    sugState = iota // no list: free text
	sugLoading                 // lookup out
	sugReady                   // all holds the values
	sugFailed                  // lookup failed or came back empty: free text
)

// maxSuggestions is how many suggestion rows are shown before "N more".
const maxSuggestions = 8

type editState struct {
	kind editKind
	v    int    // variable index (editVar)
	err  string // why the last apply was refused

	sug     sugState
	all     []string // every value, sorted
	touched bool     // the text was edited, so it filters the list
	hl      int      // highlighted entry of the filtered list, -1 for none
	gen     int      // numbers lookups so a stale result is dropped

	prevFilter string // restored by esc (editFilter)
}

// lookupKey identifies one label_values request in the cache.
func lookupKey(label, match string) string { return label + "\x00" + match }

type cachedLookup struct {
	values []string
	at     time.Time
}

// lookupResult is one fetched lookup.
type lookupResult struct {
	values []string
	err    error
}

type lookupPair struct{ label, match string }

// lookupPairs returns the label_values requests that feed suggestions for
// variable vr, or the explicit list when the dashboard gives one. Derived
// lookups drop the variable's own matcher and take other variables at their
// current values.
func (m *Model) lookupPairs(vr dashboard.Variable) (pairs []lookupPair, list []string) {
	switch vr.Mode {
	case dashboard.OptionsList:
		return nil, vr.Values
	case dashboard.OptionsLabelValues:
		return []lookupPair{{vr.Label, vr.Metric}}, nil
	case dashboard.OptionsNone:
		return nil, nil
	}
	queries := make([]string, len(m.d.Panels))
	for i, p := range m.d.Panels {
		queries[i] = p.Query
	}
	for _, s := range dashboard.SelectorsFor(vr.Name, queries) {
		label, match, err := s.Lookup(m.vals)
		if err != nil {
			continue
		}
		pr := lookupPair{label, match}
		if !slices.Contains(pairs, pr) {
			pairs = append(pairs, pr)
		}
	}
	return pairs, nil
}

// startVarEdit opens variable box i, seeded with the value in force, and
// fetches suggestions unless a fresh cached result exists.
func (m *Model) startVarEdit(i int) tea.Cmd {
	vr := m.d.Variables[i]
	m.edit = editState{kind: editVar, v: i, hl: -1, gen: m.edit.gen + 1}
	m.input.SetValue(m.vals[vr.Name])
	m.input.CursorEnd()
	focus := m.input.Focus()

	pairs, list := m.lookupPairs(vr)
	switch {
	case list != nil:
		m.setSuggestions(list)
		return focus
	case len(pairs) == 0:
		return focus
	}
	var missing []lookupPair
	for _, p := range pairs {
		if c, ok := m.lookups[lookupKey(p.label, p.match)]; !ok || m.now().Sub(c.at) > lookupTTL {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		m.assemble(pairs)
		return focus
	}
	if m.lv == nil {
		m.edit.sug = sugFailed
		return focus
	}
	m.edit.sug = sugLoading
	return tea.Batch(focus, m.lookupCmd(missing))
}

func (m *Model) lookupCmd(pairs []lookupPair) tea.Cmd {
	gen, lv, ctx := m.edit.gen, m.lv, m.rootCtx
	return func() tea.Msg {
		qctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		res := make(map[string]lookupResult, len(pairs))
		for _, p := range pairs {
			vals, err := lv.LabelValues(qctx, p.label, p.match)
			res[lookupKey(p.label, p.match)] = lookupResult{values: vals, err: err}
		}
		return lookupMsg{gen: gen, results: res}
	}
}

func (m *Model) handleLookup(msg lookupMsg) {
	if msg.gen != m.edit.gen || m.edit.kind != editVar || m.edit.sug != sugLoading {
		return
	}
	now := m.now()
	for k, r := range msg.results {
		if r.err == nil {
			m.lookups[k] = cachedLookup{values: r.values, at: now}
		}
	}
	pairs, _ := m.lookupPairs(m.d.Variables[m.edit.v])
	m.assemble(pairs)
}

// assemble merges the cached values of pairs into the suggestion list. A list
// that stays empty (failed lookups, or a label with no values) is free text.
func (m *Model) assemble(pairs []lookupPair) {
	seen := map[string]bool{}
	var all []string
	for _, p := range pairs {
		for _, v := range m.lookups[lookupKey(p.label, p.match)].values {
			if !seen[v] {
				seen[v] = true
				all = append(all, v)
			}
		}
	}
	sort.Strings(all)
	m.setSuggestions(all)
}

func (m *Model) setSuggestions(all []string) {
	if len(all) == 0 {
		m.edit.sug, m.edit.all = sugFailed, nil
		return
	}
	m.edit.sug, m.edit.all = sugReady, all
	// Start the highlight on the value in force, so down continues from it.
	m.edit.hl = -1
	if i := slices.Index(all, m.input.Value()); i >= 0 {
		m.edit.hl = i
	}
}

// filtered is the suggestion list narrowed by the typed text, case
// insensitively, once the text has been edited.
func (m *Model) filtered() []string {
	if m.edit.sug != sugReady {
		return nil
	}
	if !m.edit.touched {
		return m.edit.all
	}
	needle := strings.ToLower(m.input.Value())
	var out []string
	for _, v := range m.edit.all {
		if strings.Contains(strings.ToLower(v), needle) {
			out = append(out, v)
		}
	}
	return out
}

func (m *Model) startFilterEdit() tea.Cmd {
	m.edit = editState{kind: editFilter, prevFilter: m.rowFilter, hl: -1}
	m.input.SetValue(m.rowFilter)
	m.input.CursorEnd()
	return m.input.Focus()
}

func (m *Model) stopEdit() {
	m.edit = editState{gen: m.edit.gen}
	m.input.Blur()
	m.input.SetValue("")
}

func (m *Model) handleEditKey(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()
	if key == "ctrl+c" {
		return m.quit()
	}
	if m.edit.kind == editFilter {
		return m.handleFilterKey(msg)
	}
	list := m.filtered()
	switch key {
	case "enter":
		if m.edit.hl >= 0 && m.edit.hl < len(list) {
			m.input.SetValue(list[m.edit.hl])
		}
		cmd, _ := m.applyVar()
		return cmd
	case "esc":
		m.stopEdit()
		return nil
	case "tab", "shift+tab":
		if key == "tab" && m.edit.hl >= 0 && m.edit.hl < len(list) {
			// Accept the highlighted value into the box; a second tab moves on.
			m.input.SetValue(list[m.edit.hl])
			m.input.CursorEnd()
			m.edit.touched, m.edit.hl = false, -1
			return nil
		}
		n := len(m.d.Variables)
		next := (m.edit.v + 1) % n
		if key == "shift+tab" {
			next = (m.edit.v + n - 1) % n
		}
		// A box whose value does not validate keeps the keyboard.
		cmd, ok := m.applyVar()
		if !ok {
			return nil
		}
		return tea.Batch(cmd, m.startVarEdit(next))
	case "down", "ctrl+n":
		if len(list) > 0 {
			m.edit.hl = min(m.edit.hl+1, len(list)-1)
		}
		return nil
	case "up", "ctrl+p":
		if len(list) > 0 {
			m.edit.hl = max(m.edit.hl-1, -1)
		}
		return nil
	}
	before := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != before {
		m.edit.touched = true
	}
	// Typing is fixing what the last apply refused, and the filtered list has
	// changed under the highlight.
	m.edit.err = ""
	m.edit.hl = -1
	return cmd
}

func (m *Model) handleFilterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		m.stopEdit()
		return nil
	case "esc":
		m.rowFilter = m.edit.prevFilter
		m.stopEdit()
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.rowFilter = m.input.Value() // live: rows narrow as the user types
	return cmd
}

// applyVar validates the typed value and, when it differs from the one in
// force, applies it and re-queries every panel. ok is false when the value
// was refused; the box then stays open with the reason in edit.err.
func (m *Model) applyVar() (cmd tea.Cmd, ok bool) {
	vr := m.d.Variables[m.edit.v]
	val := m.input.Value()
	if err := vr.Check(val); err != nil {
		m.edit.err = fmt.Sprintf("invalid: $%s: %v", vr.Name, err)
		return nil, false
	}
	m.stopEdit()
	if val == m.vals[vr.Name] {
		return nil, true
	}
	m.vals[vr.Name] = val
	return m.restartQueries(), true
}
