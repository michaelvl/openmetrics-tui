// Package managed runs the managed backend: a child kubectl proxy and a child
// VictoriaMetrics wired to scrape pods through the proxy. The dashboard only
// sees the Querier.
package managed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

// Defaults.
const (
	DefaultRetention = "24h"
	logLines         = 2000
	errTail          = 15
	vmPortAttempts   = 4
)

// Options configures Start.
type Options struct {
	KubeContext string
	// Namespace overrides the scrape section's namespaces.
	Namespace  string
	KubectlBin string // default "kubectl"
	VMBin      string // default "victoria-metrics"
	// DataDir keeps history across runs; empty uses a temp dir removed on exit.
	DataDir   string
	Retention string // default DefaultRetention
	// ScrapeConfig is a user-written scrape config; ${KUBE_PROXY} is replaced
	// by the proxy address.
	ScrapeConfig string
	// LogFile receives every child output line in addition to the ring buffer.
	LogFile string
	// Scrape is the dashboard's scrape section (nil: scrape every Running pod
	// with a container port in all namespaces).
	Scrape *dashboard.Scrape
	// Interactive, when set, lets preflight commands use the terminal: stdin is
	// connected and stderr is copied here, so a login prompt is visible.
	Interactive    io.Writer
	StartupTimeout time.Duration // per child, default 30s
	ShutdownGrace  time.Duration // SIGTERM to SIGKILL, default 5s

	// wrap rewrites a tool invocation; tests use it to substitute fakes.
	wrap func(tool string, args []string) (bin string, newArgs []string)
	// skipProxy starts only VictoriaMetrics (tests with a static scrape config).
	skipProxy bool
}

// ChildStatus is the state of one child process.
type ChildStatus struct {
	Name    string
	Running bool
	Err     error // why it exited, if it did
}

// Event reports a child that exited without being asked to.
type Event struct {
	Child string
	Err   error
}

// Backend is a running managed backend.
type Backend struct {
	opts Options
	log  *LogBuffer
	logf *os.File

	tmpDir      string // removed on Close
	dataDir     string
	warnings    []string
	preflighted bool
	events      chan Event

	mu      sync.Mutex
	proxy   *child
	vm      *child
	vmAddr  string
	proxyHP string
	client  *promapi.Client
	closed  bool
	exitErr map[string]error
}

var (
	_ promapi.Querier     = (*Backend)(nil)
	_ promapi.LabelValuer = (*Backend)(nil)
)

// Start runs preflight, kubectl proxy and VictoriaMetrics. On failure it
// returns a *StartError (or a configuration error) with nothing left running.
func Start(ctx context.Context, opts Options) (*Backend, error) {
	if opts.KubectlBin == "" {
		opts.KubectlBin = "kubectl"
	}
	if opts.VMBin == "" {
		opts.VMBin = "victoria-metrics"
	}
	if opts.Retention == "" {
		opts.Retention = DefaultRetention
	}
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = 30 * time.Second
	}
	if opts.ShutdownGrace == 0 {
		opts.ShutdownGrace = 5 * time.Second
	}
	if err := ValidateRetention(opts.Retention); err != nil {
		return nil, err
	}

	b := &Backend{opts: opts, events: make(chan Event, 8), exitErr: map[string]error{}}
	var out io.Writer
	if opts.LogFile != "" {
		f, err := os.OpenFile(opts.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open --log-file: %w", err)
		}
		b.logf, out = f, f
	}
	b.log = NewLogBuffer(logLines, out)

	dir, err := os.MkdirTemp("", "omtui-dash-")
	if err != nil {
		b.closeLog()
		return nil, err
	}
	b.tmpDir = dir
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = b.Close()
		return nil, err
	}
	b.dataDir = opts.DataDir
	if b.dataDir == "" {
		b.dataDir = filepath.Join(dir, "data")
	}
	if err := os.MkdirAll(b.dataDir, 0o700); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	if err := b.startAll(ctx); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

func (b *Backend) closeLog() {
	if b.logf != nil {
		_ = b.logf.Close()
		b.logf = nil
	}
}

// command resolves a tool invocation, applying the test hook.
func (b *Backend) command(tool, bin string, args []string) (string, []string) {
	if b.opts.wrap != nil {
		return b.opts.wrap(tool, args)
	}
	return bin, args
}

func (b *Backend) fail(step string, err error) error {
	return &StartError{Step: step, Err: err, Tail: b.log.Tail(errTail)}
}

// startAll runs preflight (once per Backend) and starts both children.
func (b *Backend) startAll(ctx context.Context) error {
	if b.opts.skipProxy {
		return b.startVM(ctx, "")
	}
	if !b.preflighted {
		if err := b.preflight(ctx); err != nil {
			return err
		}
		b.preflighted = true
	}
	if err := b.startProxy(ctx); err != nil {
		return err
	}
	return b.startVM(ctx, b.proxyHP)
}

var proxyLineRe = regexp.MustCompile(`Starting to serve on (127\.0\.0\.1:\d+|\[::1\]:\d+)`)

func (b *Backend) kubectlArgs(args ...string) []string {
	if b.opts.KubeContext != "" {
		args = append(args, "--context="+b.opts.KubeContext)
	}
	return args
}

func (b *Backend) startProxy(ctx context.Context) error {
	const step = "start kubectl proxy"
	bin, args := b.command("kubectl", b.opts.KubectlBin,
		b.kubectlArgs("proxy", "--port=0", "--address=127.0.0.1"))
	hostport := make(chan string, 1)
	c, err := startChild("kubectl", bin, args, nil, b.log, func(line string) {
		if m := proxyLineRe.FindStringSubmatch(line); m != nil {
			select {
			case hostport <- m[1]:
			default:
			}
		}
	}, func(err error) { b.childExited("kubectl", err) })
	if err != nil {
		return b.fail(step, err)
	}
	var addr string
	err = c.waitReady(ctx, b.opts.StartupTimeout, func(ctx context.Context) error {
		if addr == "" {
			select {
			case addr = <-hostport:
			default:
				return errors.New("waiting for \"Starting to serve on\" line")
			}
		}
		return httpOK(ctx, "http://"+addr+"/livez")
	})
	if err != nil {
		c.stop(b.opts.ShutdownGrace)
		return b.fail(step, err)
	}
	b.mu.Lock()
	b.proxy, b.proxyHP = c, addr
	b.mu.Unlock()
	return nil
}

func (b *Backend) scrapeConfigPath(proxyHP string) (string, error) {
	var data []byte
	if b.opts.ScrapeConfig != "" {
		raw, err := os.ReadFile(b.opts.ScrapeConfig)
		if err != nil {
			return "", fmt.Errorf("read --scrape-config: %w", err)
		}
		data = []byte(strings.ReplaceAll(string(raw), "${KUBE_PROXY}", proxyHP))
	} else {
		var ns []string
		if b.opts.Namespace != "" {
			ns = []string{b.opts.Namespace}
		}
		var err error
		data, err = GenerateScrapeConfig(ScrapeParams{Scrape: b.opts.Scrape, Namespaces: ns, ProxyAddr: proxyHP})
		if err != nil {
			return "", err
		}
	}
	p := filepath.Join(b.tmpDir, "scrape.yml")
	return p, os.WriteFile(p, data, 0o600)
}

func (b *Backend) startVM(ctx context.Context, proxyHP string) error {
	const step = "start victoria-metrics"
	cfg, err := b.scrapeConfigPath(proxyHP)
	if err != nil {
		return b.fail("generate scrape config", err)
	}
	var lastErr error
	for attempt := 0; attempt < vmPortAttempts; attempt++ {
		addr := b.vmAddr
		if addr == "" || attempt > 0 {
			if addr, err = freeAddr(); err != nil {
				return b.fail(step, err)
			}
		}
		bin, args := b.command("victoria-metrics", b.opts.VMBin, []string{
			"-promscrape.config=" + cfg,
			"-storageDataPath=" + b.dataDir,
			"-httpListenAddr=" + addr,
			"-retentionPeriod=" + b.opts.Retention,
			// Show the newest sample at once; the default 30s offset would make
			// the "now" column lag by that much (see docs/managed-verification.md).
			"-search.latencyOffset=0",
		})
		c, err := startChild("victoria-metrics", bin, args, nil, b.log, nil,
			func(err error) { b.childExited("victoria-metrics", err) })
		if err != nil {
			return b.fail(step, err)
		}
		err = c.waitReady(ctx, b.opts.StartupTimeout, func(ctx context.Context) error {
			return httpOK(ctx, "http://"+addr+"/health")
		})
		if err == nil {
			client, cerr := promapi.New(promapi.Options{URL: "http://" + addr})
			if cerr != nil {
				c.stop(b.opts.ShutdownGrace)
				return b.fail(step, cerr)
			}
			b.mu.Lock()
			b.vm, b.vmAddr, b.client = c, addr, client
			b.mu.Unlock()
			return nil
		}
		c.stop(b.opts.ShutdownGrace)
		lastErr = err
		if !portClash(b.log.Tail(errTail)) {
			break
		}
		b.vmAddr = ""
	}
	return b.fail(step, lastErr)
}

var portClashRe = regexp.MustCompile(`(?i)address already in use|cannot listen|bind: `)

func portClash(tail []string) bool {
	return portClashRe.MatchString(strings.Join(tail, "\n"))
}

// freeAddr picks a loopback port by binding :0 and releasing it. Another
// process can take it before VictoriaMetrics binds, hence the retry.
func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

func httpOK(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(cctx))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}

func (b *Backend) childExited(name string, err error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.exitErr[name] = err
	b.mu.Unlock()
	select {
	case b.events <- Event{Child: name, Err: err}:
	default:
	}
}

// --- API used by the TUI ---

// Query implements promapi.Querier by delegating to the current VM client, so
// the Querier stays valid across Restart.
func (b *Backend) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]promapi.Series, error) {
	return b.current().QueryRange(ctx, expr, start, end, step)
}

// Instant implements promapi.Querier.
func (b *Backend) Instant(ctx context.Context, expr string, at time.Time) ([]promapi.Series, error) {
	return b.current().Instant(ctx, expr, at)
}

// LabelValues implements promapi.LabelValuer against the current VM client.
func (b *Backend) LabelValues(ctx context.Context, label, match string) ([]string, error) {
	return b.current().LabelValues(ctx, label, match)
}

func (b *Backend) current() *promapi.Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client
}

// Querier returns the backend as a promapi.Querier.
func (b *Backend) Querier() promapi.Querier { return b }

// URL is the VictoriaMetrics API base URL.
func (b *Backend) URL() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return "http://" + b.vmAddr
}

// Describe is a one-line description for the status line.
func (b *Backend) Describe() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctxName := b.opts.KubeContext
	if ctxName == "" {
		ctxName = "current context"
	}
	return fmt.Sprintf("managed: kubectl proxy (%s) + victoria-metrics %s", ctxName, b.vmAddr)
}

// Warnings are non-fatal findings from startup, such as an old VictoriaMetrics.
func (b *Backend) Warnings() []string { return b.warnings }

// Health reports each child's state.
func (b *Backend) Health() []ChildStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ChildStatus
	for _, c := range []*child{b.proxy, b.vm} {
		if c == nil {
			continue
		}
		out = append(out, ChildStatus{Name: c.name, Running: !c.exited(), Err: b.exitErr[c.name]})
	}
	return out
}

// Events delivers a value whenever a child exits unexpectedly.
func (b *Backend) Events() <-chan Event { return b.events }

// Logs returns the buffered child output, oldest first.
func (b *Backend) Logs() []string { return b.log.Lines() }

// Restart stops both children and starts them again, reusing the data dir and
// trying to keep the VictoriaMetrics address so the Querier stays valid. The
// proxy port changes on every start, so both children restart together.
func (b *Backend) Restart(ctx context.Context) error {
	b.stopChildren()
	b.mu.Lock()
	b.exitErr = map[string]error{}
	b.proxy, b.vm = nil, nil
	b.mu.Unlock()
	if b.opts.skipProxy {
		return b.startVM(ctx, "")
	}
	if err := b.startProxy(ctx); err != nil {
		return err
	}
	if err := b.startVM(ctx, b.proxyHP); err != nil {
		b.stopChildren()
		return err
	}
	return nil
}

func (b *Backend) stopChildren() {
	b.mu.Lock()
	vm, proxy := b.vm, b.proxy
	b.mu.Unlock()
	// Reverse of startup: VictoriaMetrics first, then the proxy it scraped through.
	if vm != nil {
		vm.stop(b.opts.ShutdownGrace)
	}
	if proxy != nil {
		proxy.stop(b.opts.ShutdownGrace)
	}
}

// Close stops the children in reverse order, then removes the temp dir. It is
// safe to call more than once.
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	b.stopChildren()
	var err error
	if b.tmpDir != "" {
		err = os.RemoveAll(b.tmpDir)
	}
	b.closeLog()
	return err
}

// --- preflight and version ---

func (b *Backend) preflight(ctx context.Context) error {
	if err := b.checkKubectl(ctx); err != nil {
		return err
	}
	if err := b.checkVM(ctx); err != nil {
		return err
	}
	ns := b.opts.Namespace
	var scope []string
	switch {
	case ns != "":
		scope = []string{"-n", ns}
	case b.opts.Scrape != nil && len(b.opts.Scrape.Namespaces) > 0:
		scope = nil // checked per namespace below
	default:
		scope = []string{"--all-namespaces"}
	}
	var namespaces [][]string
	if scope != nil {
		namespaces = [][]string{scope}
	} else {
		for _, n := range b.opts.Scrape.Namespaces {
			namespaces = append(namespaces, []string{"-n", n})
		}
	}
	for _, sc := range namespaces {
		where := "all namespaces"
		if sc[0] == "-n" {
			where = "namespace " + sc[1]
		}
		for _, chk := range []struct {
			args []string
			need string
		}{
			{[]string{"auth", "can-i", "get", "pods", "--subresource=proxy"}, "get pods/proxy"},
			{[]string{"auth", "can-i", "list", "pods"}, "list pods"},
		} {
			args := append(append([]string{}, chk.args...), sc...)
			stdout, err := b.runKubectl(ctx, args...)
			if err != nil || !strings.HasPrefix(strings.TrimSpace(stdout), "yes") {
				return b.fail("kubectl auth can-i "+strings.Join(chk.args[2:], " "),
					fmt.Errorf("missing RBAC permission to %s in %s (answer %q): managed mode scrapes pods through the API server proxy", chk.need, where, strings.TrimSpace(stdout)))
			}
		}
	}
	return nil
}

func (b *Backend) checkKubectl(ctx context.Context) error {
	const step = "kubectl version --client"
	bin, args := b.command("kubectl", b.opts.KubectlBin, []string{"version", "--client"})
	if _, err := exec.LookPath(bin); err != nil {
		return b.fail(step, fmt.Errorf("%q not found on PATH: install kubectl (https://kubernetes.io/docs/tasks/tools/) or pass --kubectl-bin", b.opts.KubectlBin))
	}
	if _, err := b.run(ctx, bin, args); err != nil {
		return b.fail(step, err)
	}
	return nil
}

func (b *Backend) checkVM(ctx context.Context) error {
	const step = "victoria-metrics -version"
	bin, args := b.command("victoria-metrics", b.opts.VMBin, []string{"-version"})
	if _, err := exec.LookPath(bin); err != nil {
		return b.fail(step, fmt.Errorf("%q not found on PATH: install VictoriaMetrics (nix: nixpkgs#victoriametrics, or https://github.com/VictoriaMetrics/VictoriaMetrics/releases) or pass --vm-bin; the Nix package of omtui-dash bundles it", b.opts.VMBin))
	}
	out, err := b.run(ctx, bin, args)
	if err != nil {
		return b.fail(step, err)
	}
	if w := CheckVersion(out); w != "" {
		b.warnings = append(b.warnings, w)
	}
	return nil
}

// runKubectl runs a kubectl command with the context flag and returns stdout.
func (b *Backend) runKubectl(ctx context.Context, args ...string) (string, error) {
	bin, a := b.command("kubectl", b.opts.KubectlBin, b.kubectlArgs(args...))
	return b.run(ctx, bin, a)
}

// run executes a short preflight command and returns its stdout. Stderr goes
// to the log and, when Interactive is set, to the terminal; stdin is only
// connected then, so a login prompt can be answered.
func (b *Backend) run(ctx context.Context, bin string, args []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // allows an interactive login
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	errw := &lineWriter{sink: func(l string) {
		b.log.Add("[preflight] " + l)
		if b.opts.Interactive != nil {
			_, _ = fmt.Fprintln(b.opts.Interactive, l)
		}
	}}
	cmd.Stderr = errw
	if b.opts.Interactive != nil {
		cmd.Stdin = os.Stdin
	}
	err := cmd.Run()
	errw.flush()
	for _, l := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if l != "" {
			b.log.Add("[preflight] " + l)
		}
	}
	return stdout.String(), err
}
