package dashtui

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/michaelvl/openmetrics-tui/internal/headless"
)

// NewSession builds the model for one dashboard session without starting a
// program, so a caller can run it with its own program options and I/O. The
// session's requests end when ctx is cancelled.
func NewSession(ctx context.Context, opts Options) *Model {
	opts.Ctx = ctx
	return New(opts)
}

// Run shows the dashboard until the user quits or ctx is cancelled. The caller
// owns the backend and releases it afterwards.
func Run(ctx context.Context, opts Options) error {
	m := NewSession(ctx, opts)
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	if opts.HTTPPort != 0 {
		addr, err := headless.Serve(ctx, opts.HTTPPort, headless.Handler(httpSource{prog.Send}))
		if err != nil {
			m.cancel()
			return fmt.Errorf("--http-port: %w", err)
		}
		fmt.Fprintf(os.Stderr, "omtui-dash: serving http://%s/screenshot and POST /variables/<name>\n", addr)
	}
	_, err := prog.Run()
	m.cancel()
	return err
}

// httpSource answers the headless server's requests through the program.
type httpSource struct{ send func(tea.Msg) }

func (s httpSource) Screenshot(ctx context.Context) (string, error) {
	return headless.Ask(ctx, s.send, func(r chan<- string) tea.Msg { return screenshotReq{r} })
}

func (s httpSource) SetVariable(ctx context.Context, name, value string) error {
	err, askErr := headless.Ask(ctx, s.send, func(r chan<- error) tea.Msg { return setVarReq{name, value, r} })
	if askErr != nil {
		return askErr
	}
	return err
}
