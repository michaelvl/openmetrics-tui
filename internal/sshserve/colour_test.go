package sshserve

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish/bubbletea"
	gossh "golang.org/x/crypto/ssh"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/dashtui"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

// emptyQuerier answers every query with no series.
type emptyQuerier struct{}

func (emptyQuerier) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]promapi.Series, error) {
	return nil, nil
}

func (emptyQuerier) Instant(context.Context, string, time.Time) ([]promapi.Series, error) {
	return nil, nil
}

const colourDash = `title: Colours
panels:
  - title: Up
    query: up
`

// newDash builds the real dashboard model with the session's own renderer, as
// omtui-dash does.
func newDash(t *testing.T) NewModel {
	d, err := dashboard.Parse([]byte(colourDash))
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, sess ssh.Session) (tea.Model, []tea.ProgramOption) {
		m := dashtui.NewSession(ctx, dashtui.Options{
			Dashboard: d, Querier: emptyQuerier{}, NoSnapshot: true, Renderer: bubbletea.MakeRenderer(sess),
		})
		return m, []tea.ProgramOption{tea.WithAltScreen()}
	}
}

// frame opens a session on c with the given TERM and returns its first frame.
func frame(t *testing.T, c *gossh.Client, term string) string {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	out := &syncBuf{}
	sess.Stdout, sess.Stderr = out, out
	if err := sess.RequestPty(term, 30, 100, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	if term != "dumb" {
		// MakeRenderer asks a real terminal for its background colour and
		// device attributes; answer the second so it stops waiting.
		waitFor(t, out, "\x1b[c")
		if _, err := in.Write([]byte("\x1b[?62;c")); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, out, "Colours") // the dashboard title
	time.Sleep(200 * time.Millisecond)
	return out.String()
}

func TestColourFollowsClientTerm(t *testing.T) {
	signer := newSigner(t)
	s := start(t, Config{AuthKeysPath: writeKeys(t, signer)}, newDash(t))
	c, err := dial(s.addr, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	colour := frame(t, c, "xterm-256color")
	plain := frame(t, c, "dumb")

	if !strings.Contains(colour, "38;5;") {
		t.Errorf("xterm-256color frame has no 256-colour sequence: %q", colour)
	}
	// Only the alt-screen and cursor control remain; no SGR colour or style.
	if strings.Contains(plain, "38;") || strings.Contains(plain, "\x1b[1m") || strings.Contains(plain, "\x1b[2m") {
		t.Errorf("dumb frame has colour or style sequences: %q", plain)
	}
}
