package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// headerHeight is the number of screen rows the header bar occupies. It never
// varies - the edit hint and any validation error go in the footer instead - so
// that the viewport is not resized under the user between keystrokes. Both
// View and the window-resize handler measure against this, so the two cannot
// drift apart.
const headerHeight = 3

// headerField names the box in the header bar that currently owns the keyboard.
// fieldNone means nobody does and the global keys are live.
type headerField int

const (
	fieldNone headerField = iota
	fieldMetricFilter
	fieldLabelFilter
	fieldAggregation
)

// editableFields is the tab order, and the single source of how many boxes the
// header draws.
var editableFields = []headerField{fieldMetricFilter, fieldLabelFilter, fieldAggregation}

func (f headerField) caption() string {
	switch f {
	case fieldMetricFilter:
		return "Metric filter"
	case fieldLabelFilter:
		return "Label filter"
	case fieldAggregation:
		return "Aggregation"
	}
	return ""
}

// shortCaption is what a box falls back to when its third of the terminal is too
// narrow for the full caption.
func (f headerField) shortCaption() string {
	switch f {
	case fieldMetricFilter:
		return "Metric"
	case fieldLabelFilter:
		return "Label"
	case fieldAggregation:
		return "Agg"
	}
	return ""
}

// key is the global key that opens the field. The box's title advertises it,
// trailing in parentheses, so the header doubles as a reminder of how to reach
// it without the help overlay.
func (f headerField) key() string {
	switch f {
	case fieldMetricFilter:
		return "m"
	case fieldLabelFilter:
		return "f"
	case fieldAggregation:
		return "a"
	}
	return ""
}

// withKey appends the field's key to a caption, as in "Metric filter (m)".
func (f headerField) withKey(caption string) string {
	return caption + " (" + f.key() + ")"
}

// step moves delta places along the tab order, wrapping at both ends so tab
// from the last field lands back on the first.
func (f headerField) step(delta int) headerField {
	for i, candidate := range editableFields {
		if candidate != f {
			continue
		}
		next := (i + delta) % len(editableFields)
		if next < 0 {
			next += len(editableFields)
		}
		return editableFields[next]
	}
	return editableFields[0]
}

// value reads the field's current applied value out of the config, so that
// opening a box always shows what is actually in force rather than whatever was
// last typed into it.
func (m model) fieldValue(f headerField) string {
	switch f {
	case fieldMetricFilter:
		return m.cfg.FilterMetric
	case fieldLabelFilter:
		return m.cfg.FilterLabel
	case fieldAggregation:
		return m.cfg.Aggregation
	}
	return ""
}

// validate reports whether a value can be applied to the field.
func validateField(f headerField, value string) error {
	switch f {
	case fieldMetricFilter:
		return validateMetricFilter(value)
	case fieldLabelFilter:
		return validateLabelFilter(value)
	case fieldAggregation:
		return validateAggregation(value)
	}
	return nil
}

// startEditing gives a header box the keyboard, seeded with the value currently
// in force.
func (m *model) startEditing(field headerField) tea.Cmd {
	m.editing = field
	m.inputErr = ""
	m.input.SetValue(m.fieldValue(field))
	m.input.CursorEnd()
	return m.input.Focus()
}

// commitEditing applies what was typed, reporting whether it took. A value that
// does not validate leaves the config untouched and the field focused, so the
// user can fix it in place rather than losing what they typed.
func (m *model) commitEditing() bool {
	if m.editing == fieldNone {
		return true
	}
	value := m.input.Value()
	if err := validateField(m.editing, value); err != nil {
		m.inputErr = err.Error()
		return false
	}

	switch m.editing {
	case fieldMetricFilter:
		m.cfg.FilterMetric = value
	case fieldLabelFilter:
		m.cfg.FilterLabel = value
	case fieldAggregation:
		m.cfg.Aggregation = value
	}

	m.inputErr = ""
	m.stopEditing()
	m.refresh()
	return true
}

// cancelEditing drops what was typed, leaving the config as it was.
func (m *model) cancelEditing() {
	m.inputErr = ""
	m.stopEditing()
}

func (m *model) stopEditing() {
	m.editing = fieldNone
	m.input.Blur()
	m.input.SetValue("")
}

// moveField applies the current box and moves to the next one along the tab
// order. A box whose value does not validate keeps the keyboard, so tab cannot
// carry a broken regex out of sight.
func (m *model) moveField(delta int) tea.Cmd {
	if m.editing == fieldNone {
		return nil
	}
	next := m.editing.step(delta)
	if !m.commitEditing() {
		return nil
	}
	return m.startEditing(next)
}

// renderHeader draws the three boxes side by side, always headerHeight rows
// tall. The boxes split the terminal width evenly, with the remainder going to
// the last one so the bar ends flush with the right edge.
func (m model) renderHeader() string {
	widths := headerBoxWidths(m.width, len(editableFields))

	boxes := make([]string, 0, len(editableFields))
	for i, field := range editableFields {
		boxes = append(boxes, m.renderHeaderBox(field, widths[i]))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, boxes...)
}

// headerBoxWidths splits total columns over n boxes, giving the remainder to the
// last. A box narrower than its border cannot be drawn, so the minimum is the
// two border columns plus one for content.
func headerBoxWidths(total, n int) []int {
	if total < n*3 {
		total = n * 3
	}
	each := total / n
	widths := make([]int, n)
	for i := range widths {
		widths[i] = each
	}
	widths[n-1] += total - each*n
	return widths
}

// renderHeaderBox draws one box at an exact outer width, with the caption set
// into its top border as a legend. lipgloss has no border-title API, so the top
// edge is drawn by hand and the rest of the box by lipgloss with its top border
// switched off. Every path through here produces exactly one content line: a
// line that overflows would wrap and push the viewport down a row, which is the
// one thing the header must never do.
func (m model) renderHeaderBox(field headerField, width int) string {
	focused := m.editing == field

	border := lipgloss.Color("240")
	switch {
	case focused && m.inputErr != "":
		border = lipgloss.Color("196")
	case focused:
		border = lipgloss.Color("63")
	}

	inner := width - 2 // the border's two columns
	if inner < 1 {
		inner = 1
	}

	var value string
	if focused {
		// textinput spends one column on the cursor, and re-measures its own
		// scroll window only when the cursor moves.
		m.input.Width = max(1, inner-1)
		m.input.SetCursor(m.input.Position())
		value = m.input.View()
	} else if raw := m.fieldValue(field); raw == "" {
		value = lipgloss.NewStyle().Faint(true).Render(truncateToWidth("—", inner))
	} else {
		value = truncateToWidth(raw, inner)
	}

	captionStyle := lipgloss.NewStyle().Faint(true)
	if focused {
		captionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	}

	top := headerBoxTop(field, inner, lipgloss.NewStyle().Foreground(border), captionStyle)
	body := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderTop(false).
		BorderForeground(border).
		Width(inner).
		MaxHeight(headerHeight - 1).
		Render(value)
	return top + "\n" + body
}

// headerBoxTop draws a box's top border with the caption set into it, as in
// ╭─┤ Metric filter (m) ├──────╮. The caption shortens, keeping the key, then
// goes entirely, as the box narrows past the point where it fits with some
// border left either side.
func headerBoxTop(field headerField, inner int, borderStyle, captionStyle lipgloss.Style) string {
	b := lipgloss.RoundedBorder()
	// "─┤ " before the caption, " ├" after it, and at least one "─" to close.
	const legendOverhead = 6

	caption := field.withKey(field.caption())
	if inner-lipgloss.Width(caption) < legendOverhead {
		caption = field.withKey(field.shortCaption())
	}
	if inner-lipgloss.Width(caption) < legendOverhead {
		return borderStyle.Render(b.TopLeft + strings.Repeat(b.Top, inner) + b.TopRight)
	}

	fill := inner - lipgloss.Width(caption) - legendOverhead + 1
	return borderStyle.Render(b.TopLeft+b.Top+"┤ ") +
		captionStyle.Render(caption) +
		borderStyle.Render(" ├"+strings.Repeat(b.Top, fill)+b.TopRight)
}

// truncateToWidth cuts a string to at most width display cells, marking the cut
// with an ellipsis. Unlike truncateMessage, which the footer uses, it honours
// widths below four - the header's boxes get a third of the terminal each and
// can be very narrow.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(s)
	out := make([]rune, 0, len(runes))
	used := 0
	for _, r := range runes {
		w := lipgloss.Width(string(r))
		if used+w > width-1 {
			break
		}
		out = append(out, r)
		used += w
	}
	return string(out) + "…"
}

// minHintWidth is the least the edit hint will shrink to. Below it the hint says
// nothing at all, and a terminal that narrow has bigger problems.
const minHintWidth = 12

// statusReserveWhileEdit is the room the edit hint leaves for the endpoint URL
// at the other end of the footer. It is deliberately meagre: while a header box
// is open the user is reading the hint, not the endpoint.
const statusReserveWhileEdit = 12

// minStatusWidth is the room the footer insists on leaving for the endpoint or
// the connection error before it starts dropping mode indicators.
const minStatusWidth = 12

// headerHint is the footer's left-hand segment while a box is being edited. It
// carries the validation error when there is one, because the header itself is
// a fixed height with no room for it. width is what the rest of the footer can
// spare; a regex error is as long as the regex, so it has to be cut somewhere.
func (m model) headerHint(width int) string {
	text := "enter: apply · esc: cancel · tab: next field"
	style := lipgloss.NewStyle().Faint(true)
	if m.inputErr != "" {
		text = fmt.Sprintf("⚠ %s: %s", m.editing.caption(), m.inputErr)
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	}
	return style.Render(truncateToWidth(text, max(width, minHintWidth)))
}
