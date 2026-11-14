// Package dashtui is the terminal UI of omtui-dash: a Bubble Tea model that
// renders a dashboard definition as stacked panels of rows and window
// columns, refreshed from a promapi.Querier. It works the same against an
// external backend and a managed one; the only difference is the optional
// Backend used for child health, logs and restart.
package dashtui

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/managed"
	"github.com/michaelvl/openmetrics-tui/internal/panel"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

// Backend is the part of managed.Backend the UI needs: child health, the log
// ring buffer and restart. It is nil for an external backend.
type Backend interface {
	Health() []managed.ChildStatus
	Events() <-chan managed.Event
	Logs() []string
	Restart(ctx context.Context) error
}

// Options configures a Model.
type Options struct {
	Dashboard *dashboard.Dashboard
	// Values are the initial variable values (defaults plus --var).
	Values  map[string]string
	Querier promapi.Querier
	// Mode is shown in the status line: "external <url>" or "managed <ctx>".
	Mode string
	// KubeContext is shown at the left of the footer in managed mode.
	KubeContext string
	// URL is the external Prometheus API, shown after the footer status.
	URL string
	// Backend is set in managed mode.
	Backend Backend
	// Ctx bounds the lifetime of every request; nil means Background.
	Ctx context.Context
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// SnapshotDir is where S writes its file; empty means the working
	// directory.
	SnapshotDir string
}

const (
	// planWidth is the number of points a panel range is planned for; the
	// sparkline resamples them, the window columns pick from them.
	planWidth = 120
	// minStep is the smallest query step; scrape intervals are rarely shorter.
	minStep      = 15 * time.Second
	queryTimeout = 30 * time.Second
	maxBackoff   = 2 * time.Minute
	// lookupTTL is how long a label_values result is reused.
	lookupTTL = 30 * time.Second
	// lookupTimeout bounds a suggestion lookup so a slow backend only costs
	// the user free text.
	lookupTimeout = 10 * time.Second
)

// panelState is one panel's definition plus everything that changes at
// runtime.
type panelState struct {
	def    dashboard.Panel
	folded bool

	// loaded is set once the first response (or error) arrived for the
	// current variable values.
	loaded   bool
	err      error
	rows     []panel.Row
	rng      panel.Range
	inflight bool
	failures int
	retryAt  time.Time

	// shown is rows after sort, limit and the row filter; rebuilt on every
	// update.
	shown []panel.Row
}

// Messages.
type (
	tickMsg  struct{ gen int }
	panelMsg struct {
		gen, idx int
		rows     []panel.Row
		rng      panel.Range
		at       time.Time
		err      error
	}
	healthMsg  struct{ status []managed.ChildStatus }
	eventMsg   struct{ ev managed.Event }
	logsMsg    struct{ lines []string }
	restartMsg struct{ err error }
	lookupMsg  struct {
		gen     int
		results map[string]lookupResult
	}
	clearSnapshotMsg struct{ gen int }
)

// Model is the Bubble Tea model. It is used through a pointer so panel state
// can be shared between the update loop and the commands it spawns.
type Model struct {
	d       *dashboard.Dashboard
	q       promapi.Querier
	lv      promapi.LabelValuer
	backend Backend
	mode    string
	kubeCtx string
	url     string
	now     func() time.Time
	snapDir string
	// tickFn schedules a tick; tests replace it so no timer runs.
	tickFn func(time.Duration, func(time.Time) tea.Msg) tea.Cmd

	vals     map[string]string
	interval time.Duration
	paused   bool
	width    int
	height   int
	panels   []*panelState
	active   int
	sortMode panel.SortMode
	// rowFilter is the substring row filter, applied to every panel.
	rowFilter string
	showHelp  bool
	lastOK    time.Time

	// rootCtx lives as long as the program; ctx is the current generation's
	// context. A variable change cancels ctx, which aborts in-flight
	// requests, and bumps gen so their late results are dropped. tickGen
	// numbers the tick chain the same way (the interval changes it too).
	rootCtx context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	gen     int
	tickGen int

	// input is the one text field shared by the variable boxes and the row
	// filter; edit says who owns it.
	input textinput.Model
	edit  editState
	// lookups caches label_values results for lookupTTL.
	lookups map[string]cachedLookup

	// Managed backend state.
	health     []managed.ChildStatus
	restarting bool
	restartErr string
	logOpen    bool
	logs       []string
	logScroll  int // lines scrolled up from the newest

	snapshotMsg string
	snapshotErr bool
	snapshotGen int

	// Layout, rebuilt after every update.
	lines        []string
	shownWindows []int // window columns the grid still has room for
	titleLine    []int
	scroll       int
	follow       bool // scroll to keep the active panel visible
	bannerLines  []string
}

var _ tea.Model = (*Model)(nil)

// New builds a Model. It does no I/O.
func New(opts Options) *Model {
	d := opts.Dashboard
	root := opts.Ctx
	if root == nil {
		root = context.Background()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	vals := map[string]string{}
	for k, v := range opts.Values {
		vals[k] = v
	}
	for _, v := range d.Variables {
		if _, ok := vals[v.Name]; !ok {
			vals[v.Name] = v.Default
		}
	}

	// A static cursor keeps the input out of the message loop: a blinking one
	// would need cursor.BlinkMsg plumbed through Update, as the existing tool
	// also avoids.
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 256
	in.Cursor.SetMode(cursor.CursorStatic)

	m := &Model{
		d:        d,
		q:        opts.Querier,
		backend:  opts.Backend,
		mode:     opts.Mode,
		kubeCtx:  opts.KubeContext,
		url:      opts.URL,
		now:      now,
		snapDir:  opts.SnapshotDir,
		tickFn:   tea.Tick,
		vals:     vals,
		interval: d.Refresh,
		width:    80,
		height:   24,
		rootCtx:  root,
		input:    in,
		lookups:  map[string]cachedLookup{},
	}
	if m.interval <= 0 {
		m.interval = dashboard.DefaultRefresh
	}
	if lv, ok := opts.Querier.(promapi.LabelValuer); ok {
		m.lv = lv
	}
	m.ctx, m.cancel = context.WithCancel(root)
	for _, p := range d.Panels {
		m.panels = append(m.panels, &panelState{def: p, folded: p.Folded})
	}
	m.rebuild()
	return m
}

// Init starts the first round of queries and the tick chain.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.tickCmd()}
	cmds = append(cmds, m.dispatchAll(true)...)
	if m.backend != nil {
		cmds = append(cmds, m.healthCmd(), m.waitEvent())
	}
	return tea.Batch(cmds...)
}

func (m *Model) tickCmd() tea.Cmd {
	gen := m.tickGen
	return m.tickFn(m.interval, func(time.Time) tea.Msg { return tickMsg{gen: gen} })
}

// dispatchAll queries every panel that has no request out. With force, a panel
// in back-off is queried too.
func (m *Model) dispatchAll(force bool) []tea.Cmd {
	var cmds []tea.Cmd
	now := m.now()
	for i, p := range m.panels {
		if p.inflight || (!force && now.Before(p.retryAt)) {
			continue
		}
		cmds = append(cmds, m.dispatch(i))
	}
	return cmds
}

// dispatch builds the command that queries panel i. The query and range are
// fixed here, on the update goroutine; the command itself touches no model
// state.
func (m *Model) dispatch(i int) tea.Cmd {
	p := m.panels[i]
	p.inflight = true
	gen, ctx, q := m.gen, m.ctx, m.q
	end := m.now()
	rng := panel.PlanRange(m.d.Windows, m.d.Spark, end, planWidth, minStep)
	query, subErr := dashboard.Substitute(p.def.Query, m.vals)
	def, windows, spark := p.def, m.d.Windows, m.d.Spark
	return func() tea.Msg {
		if subErr != nil {
			return panelMsg{gen: gen, idx: i, err: subErr}
		}
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		series, err := q.QueryRange(qctx, query, rng.Start, rng.End, rng.Step)
		if err != nil {
			return panelMsg{gen: gen, idx: i, err: err}
		}
		return panelMsg{gen: gen, idx: i, rows: panel.Extract(def, series, windows, spark, rng), rng: rng, at: end}
	}
}

// restartQueries throws away what the panels show and queries them again,
// cancelling requests still out for the old values. Used when a variable
// changes.
func (m *Model) restartQueries() tea.Cmd {
	m.cancel()
	m.ctx, m.cancel = context.WithCancel(m.rootCtx)
	m.gen++
	m.tickGen++
	for _, p := range m.panels {
		*p = panelState{def: p.def, folded: p.folded}
	}
	cmds := m.dispatchAll(true)
	cmds = append(cmds, m.tickCmd())
	return tea.Batch(cmds...)
}

// backoff is how long a panel waits after its n-th consecutive failure:
// the refresh interval doubled each time, up to maxBackoff.
func (m *Model) backoff(failures int) time.Duration {
	d := m.interval
	for i := 0; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, max(maxBackoff, m.interval))
}

func (m *Model) healthCmd() tea.Cmd {
	b := m.backend
	return func() tea.Msg { return healthMsg{status: b.Health()} }
}

func (m *Model) waitEvent() tea.Cmd {
	ch, ctx := m.backend.Events(), m.rootCtx
	return func() tea.Msg {
		select {
		case ev := <-ch:
			return eventMsg{ev: ev}
		case <-ctx.Done():
			return nil
		}
	}
}

// Update handles messages. The layout is rebuilt afterwards so View is a pure
// slice of cached lines.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.rebuild()
	return m, cmd
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.follow = true
	case tickMsg:
		if msg.gen != m.tickGen {
			return nil // a chain from before the interval or variables changed
		}
		cmds := []tea.Cmd{m.tickCmd()}
		if !m.paused {
			cmds = append(cmds, m.dispatchAll(false)...)
		}
		if m.backend != nil {
			cmds = append(cmds, m.healthCmd())
			if m.logOpen {
				cmds = append(cmds, m.logsCmd())
			}
		}
		return tea.Batch(cmds...)
	case panelMsg:
		m.handlePanel(msg)
	case healthMsg:
		m.health = msg.status
	case eventMsg:
		return tea.Batch(m.healthCmd(), m.waitEvent())
	case logsMsg:
		m.logs = msg.lines
	case restartMsg:
		m.restarting = false
		m.restartErr = ""
		if msg.err != nil {
			m.restartErr = msg.err.Error()
		}
		cmds := m.dispatchAll(true)
		return tea.Batch(append(cmds, m.healthCmd())...)
	case lookupMsg:
		m.handleLookup(msg)
	case clearSnapshotMsg:
		if msg.gen == m.snapshotGen {
			m.snapshotMsg = ""
		}
	}
	return nil
}

func (m *Model) handlePanel(msg panelMsg) {
	if msg.gen != m.gen || msg.idx >= len(m.panels) {
		return // from before the variables changed
	}
	p := m.panels[msg.idx]
	p.inflight = false
	p.loaded = true
	if msg.err != nil {
		p.err = msg.err
		p.rows = nil
		p.failures++
		p.retryAt = m.now().Add(m.backoff(p.failures))
		return
	}
	p.err, p.failures, p.retryAt = nil, 0, time.Time{}
	p.rows, p.rng = msg.rows, msg.rng
	if msg.at.After(m.lastOK) {
		m.lastOK = msg.at
	}
}

func (m *Model) logsCmd() tea.Cmd {
	b := m.backend
	return func() tea.Msg { return logsMsg{lines: b.Logs()} }
}

func (m *Model) restartCmd() tea.Cmd {
	b, ctx := m.backend, m.rootCtx
	return func() tea.Msg { return restartMsg{err: b.Restart(ctx)} }
}

// intervalSteps is the ladder + and - move the refresh interval along.
var intervalSteps = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second,
	30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
}

// stepInterval moves cur one step along intervalSteps, shorter when faster. An
// interval off the ladder joins it at the nearest step in that direction.
func stepInterval(cur time.Duration, faster bool) time.Duration {
	if faster {
		for i := len(intervalSteps) - 1; i >= 0; i-- {
			if intervalSteps[i] < cur {
				return intervalSteps[i]
			}
		}
		return cur
	}
	for _, s := range intervalSteps {
		if s > cur {
			return s
		}
	}
	return cur
}
