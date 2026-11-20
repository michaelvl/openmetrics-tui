package sshserve

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/bubbletea"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/dashtui"
)

// CatalogModel serves the dashboards of cat. The catalog is re-read on every
// connection. The dashboard comes from the command, as in `ssh -t host NAME`;
// without one, a single dashboard opens at once and several show a picker.
// base carries everything but the dashboard, its values and the renderer.
func CatalogModel(cat *dashboard.Catalog, base dashtui.Options) NewModel {
	return func(ctx context.Context, sess ssh.Session) (tea.Model, []tea.ProgramOption) {
		for _, err := range cat.Reload() {
			log.Printf("dashboards: %v", err)
		}
		args := sess.Command()
		if len(args) > 1 {
			fail(sess, "Usage: ssh -t host [NAME]")
			return nil, nil
		}
		names := cat.Names()
		// The renderer takes its colour profile from the client's TERM, not the
		// server's stdout. Wish sets the program's input and output itself.
		// Making it probes the terminal, so it waits until a session starts.
		renderer := sync.OnceValue(func() *lipgloss.Renderer { return bubbletea.MakeRenderer(sess) })
		open := func(d *dashboard.Dashboard) (tea.Model, error) {
			vals, err := d.Values(nil)
			if err != nil {
				return nil, err
			}
			opts := base
			opts.Dashboard, opts.Values, opts.Renderer = d, vals, renderer()
			return dashtui.NewSession(ctx, opts), nil
		}

		var m tea.Model
		switch {
		case len(args) == 1:
			d, ok := cat.Get(args[0])
			if !ok {
				fail(sess, fmt.Sprintf("Unknown dashboard %q. Available: %s", args[0], strings.Join(names, ", ")))
				return nil, nil
			}
			var err error
			if m, err = open(d); err != nil {
				fail(sess, err.Error())
				return nil, nil
			}
		case len(names) == 1:
			d, _ := cat.Get(names[0])
			var err error
			if m, err = open(d); err != nil {
				fail(sess, err.Error())
				return nil, nil
			}
		default:
			entries := make([]pickEntry, 0, len(names))
			for _, n := range names {
				if d, ok := cat.Get(n); ok {
					entries = append(entries, pickEntry{name: n, d: d})
				}
			}
			m = &picker{entries: entries, open: open, r: renderer()}
		}
		return m, []tea.ProgramOption{tea.WithAltScreen()}
	}
}

// fail tells the client why the session ends and exits non-zero. The line ends
// in CR LF because the client's terminal is in raw mode.
func fail(sess ssh.Session, msg string) {
	wish.Print(sess, msg+"\r\n")
	_ = sess.Exit(1)
}

type pickEntry struct {
	name string
	d    *dashboard.Dashboard
}

// picker lists the dashboards and hands over to the chosen one on Enter.
type picker struct {
	entries []pickEntry
	open    func(*dashboard.Dashboard) (tea.Model, error)
	r       *lipgloss.Renderer
	cursor  int
	w, h    int
	err     error
}

func (p *picker) Init() tea.Cmd { return nil }

func (p *picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.w, p.h = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return p, tea.Quit
		case "up", "k":
			p.cursor = max(p.cursor-1, 0)
		case "down", "j":
			p.cursor = min(p.cursor+1, len(p.entries)-1)
		case "enter":
			m, err := p.open(p.entries[p.cursor].d)
			if err != nil {
				p.err = err
				return p, nil
			}
			// The new model has not seen the window size yet.
			size := tea.WindowSizeMsg{Width: p.w, Height: p.h}
			return m, tea.Batch(m.Init(), func() tea.Msg { return size })
		}
	}
	return p, nil
}

func (p *picker) View() string {
	accent := p.r.NewStyle().Foreground(lipgloss.Color("63"))
	faint := p.r.NewStyle().Faint(true)
	var b strings.Builder
	b.WriteString(p.r.NewStyle().Bold(true).Render("Choose a dashboard") + "\n\n")
	for i, e := range p.entries {
		line := fmt.Sprintf("%s  %s", e.d.Title, faint.Render("("+e.name+")"))
		if i == p.cursor {
			line = accent.Render("> ") + line
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	if p.err != nil {
		b.WriteString("\n" + p.r.NewStyle().Foreground(lipgloss.Color("1")).Render(p.err.Error()) + "\n")
	}
	b.WriteString("\n" + faint.Render("up/down select, enter open, q quit"))
	return b.String()
}
