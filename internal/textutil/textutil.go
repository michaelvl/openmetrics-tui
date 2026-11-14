package textutil

import "github.com/charmbracelet/lipgloss"

// TruncateToWidth cuts a string to at most width display cells, marking the cut
// with an ellipsis. Unlike truncateMessage, which the footer uses, it honours
// widths below four - the header's boxes get a third of the terminal each and
// can be very narrow.
func TruncateToWidth(s string, width int) string {
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
