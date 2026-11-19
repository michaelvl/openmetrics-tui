package dashtui

import (
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// rendererModel is demoModel with a renderer pinned to profile p.
func rendererModel(t *testing.T, p termenv.Profile) *Model {
	t.Helper()
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(p)
	m := newTestModel(t, mustDash(t, testDash), &fakeQuerier{handle: demoHandler}, func(o *Options) { o.Renderer = r })
	start(m)
	return m
}

func TestViewColourFollowsRendererProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile termenv.Profile
		want    []string
		notWant []string
	}{
		{"Ascii", termenv.Ascii, nil, []string{"\x1b["}},
		{"ANSI", termenv.ANSI, []string{"\x1b["}, []string{"38;5;", "38;2;"}},
		{"ANSI256", termenv.ANSI256, []string{"38;5;"}, []string{"38;2;"}},
		{"TrueColor", termenv.TrueColor, []string{"38;2;"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := rendererModel(t, tt.profile)
			v := m.View()
			for _, s := range tt.want {
				if !strings.Contains(v, s) {
					t.Errorf("view lacks %q", s)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(v, s) {
					t.Errorf("view contains %q", s)
				}
			}
			// The help overlay builds its own styles.
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
			if help := m.View(); strings.Contains(help, "\x1b[") != (tt.profile != termenv.Ascii) {
				t.Errorf("help overlay colour does not match profile %s", tt.name)
			}
		})
	}
}

func TestRenderersDoNotAffectEachOther(t *testing.T) {
	colour := rendererModel(t, termenv.ANSI256)
	plain := rendererModel(t, termenv.Ascii)
	// Interleave the views so any shared state would show.
	for range 2 {
		if v := colour.View(); !strings.Contains(v, "38;5;") {
			t.Error("256-colour model lost its colours")
		}
		if v := plain.View(); strings.Contains(v, "\x1b[") {
			t.Error("plain model picked up escape sequences")
		}
	}
}

func TestNoRendererUsesDefault(t *testing.T) {
	// TestMain pins the default renderer to Ascii.
	m := demoModel(t, 100, 40)
	if strings.Contains(m.View(), "\x1b[") {
		t.Error("default renderer should be colourless in tests")
	}
}
