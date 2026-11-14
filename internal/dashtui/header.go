package dashtui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/prometheus/common/model"

	"github.com/michaelvl/openmetrics-tui/internal/textutil"
)

// headerHeight is the rows the header bar occupies. It never varies: the edit
// hint, validation errors and the suggestion list all live outside it, so the
// panel area is not resized between keystrokes.
const (
	titleHeight  = 1 // the dashboard title, on a line by itself
	boxHeight    = 3 // variable boxes and the status lines beside them
	headerHeight = titleHeight + boxHeight
)

const (
	maxBoxW = 26
	minBoxW = 12
	// The status lines keep between minStatusW and maxStatusW next to the boxes.
	minStatusW = 20
	maxStatusW = 36
)

type boxPos struct{ v, x, w int }

// headerBoxes places the variable boxes. Boxes shrink to share the room left of
// the status lines; once they would be narrower than minBoxW the ones that do
// not fit are left out (their number keys still work), except the box being
// edited, which takes the last slot so it is always in view.
func (m *Model) headerBoxes() (boxes []boxPos, hidden int) {
	n := len(m.d.Variables)
	if n == 0 {
		return nil, 0
	}
	// The status lines get less room on a narrow terminal, so the boxes keep
	// legible captions for as long as possible.
	avail := max(m.width-max(minStatusW, min(maxStatusW, m.width*2/5)), minBoxW)
	w := min(maxBoxW, avail/n)
	visible := make([]int, 0, n)
	if w >= minBoxW {
		for i := range n {
			visible = append(visible, i)
		}
	} else {
		w = minBoxW
		k := max(1, avail/minBoxW)
		for i := range min(k, n) {
			visible = append(visible, i)
		}
		if m.edit.kind == editVar && m.edit.v >= k {
			visible[len(visible)-1] = m.edit.v
		}
		hidden = n - len(visible)
	}
	for k, v := range visible {
		boxes = append(boxes, boxPos{v: v, x: k * w, w: w})
	}
	return boxes, hidden
}

func (m *Model) renderHeader() string {
	boxes, hidden := m.headerBoxes()
	rows := []string{"", "", ""}
	boxesW := 0
	if len(boxes) > 0 {
		var cols [][]string
		for _, b := range boxes {
			cols = append(cols, strings.Split(m.renderBox(b), "\n"))
			boxesW += b.w
		}
		for r := range rows {
			for _, c := range cols {
				rows[r] += c[r]
			}
		}
	}
	statusW := m.width - boxesW - 2
	status := []string{"", "", ""}
	if hidden > 0 {
		status[0] = faintStyle.Render(fmt.Sprintf("+%d more variables (keys %s)", hidden, m.hiddenKeys(boxes)))
	}
	for r := range rows {
		if statusW > 0 && status[r] != "" {
			rows[r] += "  " + clip(status[r], statusW)
		}
	}
	title := dashTitleStyle.Render(textutil.TruncateToWidth(m.d.Title, m.width))
	return title + "\n" + strings.Join(rows, "\n")
}

func (m *Model) hiddenKeys(boxes []boxPos) string {
	shown := map[int]bool{}
	for _, b := range boxes {
		shown[b.v] = true
	}
	var keys []string
	for i := range m.d.Variables {
		if !shown[i] {
			keys = append(keys, fmt.Sprint(i+1))
		}
	}
	return strings.Join(keys, ",")
}

// renderBox draws one variable box at an exact outer width with the caption set
// into its top border, as the existing tool's header boxes do. The top edge is
// drawn by hand; lipgloss draws the rest with its top border off.
func (m *Model) renderBox(b boxPos) string {
	vr := m.d.Variables[b.v]
	focused := m.edit.kind == editVar && m.edit.v == b.v

	border := lipgloss.Color("240")
	switch {
	case focused && m.edit.err != "":
		border = lipgloss.Color(colRed)
	case focused:
		border = lipgloss.Color("63")
	}
	inner := max(1, b.w-2)

	var value string
	switch {
	case focused:
		in := m.input
		in.Width = max(1, inner-1)
		in.SetCursor(in.Position())
		value = in.View()
	case m.vals[vr.Name] == "":
		value = faintStyle.Render(textutil.TruncateToWidth("—", inner))
	default:
		value = textutil.TruncateToWidth(m.vals[vr.Name], inner)
	}

	captionStyle := faintStyle
	if focused {
		captionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	}
	top := boxTop(vr.Name, b.v+1, inner, lipgloss.NewStyle().Foreground(border), captionStyle)
	body := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderTop(false).
		BorderForeground(border).
		Width(inner).
		MaxHeight(boxHeight - 1).
		Render(value)
	return top + "\n" + body
}

// boxTop draws ┌─┤ $name (1) ├──┐ with the caption shortened, keeping the key, and
// finally dropped as the box narrows.
func boxTop(name string, n, inner int, borderStyle, captionStyle lipgloss.Style) string {
	b := lipgloss.RoundedBorder()
	// "─┤ " before the caption, " ├" after it, and at least one "─" to close.
	const overhead = 6
	caption := fmt.Sprintf("$%s (%d)", name, n)
	if inner-lipgloss.Width(caption) < overhead {
		// Cut the name, not the key: "$names… (1)".
		room := inner - overhead - lipgloss.Width(fmt.Sprintf(" (%d)", n)) - 1
		caption = "$" + textutil.TruncateToWidth(name, max(room, 1)) + fmt.Sprintf(" (%d)", n)
	}
	if inner-lipgloss.Width(caption) < overhead {
		caption = fmt.Sprintf("(%d)", n)
	}
	if inner-lipgloss.Width(caption) < overhead {
		return borderStyle.Render(b.TopLeft + strings.Repeat(b.Top, inner) + b.TopRight)
	}
	fill := inner - lipgloss.Width(caption) - overhead + 1
	return borderStyle.Render(b.TopLeft+b.Top+"┤ ") +
		captionStyle.Render(caption) +
		borderStyle.Render(" ├"+strings.Repeat(b.Top, fill)+b.TopRight)
}

// formatInterval renders a refresh interval compactly: 500ms, 10s, 2m.
func formatInterval(d time.Duration) string { return model.Duration(d).String() }

// overlaySuggestions draws the suggestion list over the top of the panel area,
// under the box being edited. It shows at most maxSuggestions entries around
// the highlight and an "N more" line for the rest.
func (m *Model) overlaySuggestions(body []string) []string {
	list := m.filtered()
	if m.edit.kind != editVar || len(list) == 0 {
		return body
	}
	boxes, _ := m.headerBoxes()
	var pos *boxPos
	for i := range boxes {
		if boxes[i].v == m.edit.v {
			pos = &boxes[i]
		}
	}
	if pos == nil {
		return body
	}
	w := min(pos.w, m.width-pos.x)
	inner := max(1, w-2)

	start := 0
	if m.edit.hl >= maxSuggestions {
		start = m.edit.hl - maxSuggestions + 1
	}
	end := min(start+maxSuggestions, len(list))
	var rows []string
	for i := start; i < end; i++ {
		mark, text := "  ", textutil.TruncateToWidth(list[i], inner-2)
		if i == m.edit.hl {
			mark, text = "▸ ", accentStyle.Render(text)
		}
		rows = append(rows, mark+text)
	}
	if more := len(list) - len(rows); more > 0 {
		rows = append(rows, faintStyle.Render(fmt.Sprintf("… %d more", more)))
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63")).
		Width(inner).
		Render(strings.Join(rows, "\n"))
	for j, ol := range strings.Split(box, "\n") {
		if j >= len(body) {
			break
		}
		base := padRight(body[j], pos.x+w)
		body[j] = ansi.Truncate(base, pos.x, "") + ol + ansi.TruncateLeft(base, pos.x+lipgloss.Width(ol), "")
	}
	return body
}
