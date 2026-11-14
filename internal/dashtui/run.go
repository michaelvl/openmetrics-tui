package dashtui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// Run shows the dashboard until the user quits or ctx is cancelled. The caller
// owns the backend and releases it afterwards.
func Run(ctx context.Context, opts Options) error {
	opts.Ctx = ctx
	m := New(opts)
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	m.cancel()
	return err
}
