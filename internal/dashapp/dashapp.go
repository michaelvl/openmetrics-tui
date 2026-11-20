// Package dashapp is the omtui-dash command: flag handling, backend
// selection (external or managed), the --check mode and the TUI launch.
package dashapp

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish/bubbletea"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/dashtui"
	"github.com/michaelvl/openmetrics-tui/internal/managed"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
	"github.com/michaelvl/openmetrics-tui/internal/sshserve"
)

// varFlags collects repeated --var name=value flags.
type varFlags map[string]string

func (v varFlags) String() string { return fmt.Sprint(map[string]string(v)) }

func (v varFlags) Set(s string) error {
	name, val, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return fmt.Errorf("want name=value, got %q", s)
	}
	v[name] = val
	return nil
}

// stringsFlag collects a repeated string flag.
type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, ",") }

func (f *stringsFlag) Set(s string) error {
	*f = append(*f, s)
	return nil
}

// managedOnlyFlags are meaningless without --managed.
var managedOnlyFlags = []string{
	"kube-context", "namespace", "kubectl-bin", "vm-bin",
	"data-dir", "retention", "scrape-config", "log-file", "in-cluster",
}

type config struct {
	check    bool
	vars     varFlags
	promURL  string
	opts     promapi.Options
	managed  bool
	httpPort int
	mopts    managed.Options
	ssh      sshserve.Config
}

// sshOnlyFlags are meaningless without --ssh-listen.
var sshOnlyFlags = []string{
	"ssh-host-key", "ssh-authorized-keys", "ssh-allow-any-key", "ssh-max-sessions",
}

// defaultHostKey is where the SSH host key is kept unless --ssh-host-key says
// otherwise.
const defaultHostKey = "omtui-dash-host-key"

// Main runs the command and exits the process.
func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run is Main without the exit, returning the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("omtui-dash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config{vars: varFlags{}}
	var printScrape bool
	fs.BoolVar(&cfg.check, "check", false, "validate the dashboard and run every query once, then exit")
	fs.Var(cfg.vars, "var", "set a variable, `name=value` (repeatable)")
	fs.StringVar(&cfg.promURL, "prom", "", "Prometheus-compatible API `URL` (overrides datasource.url)")
	fs.StringVar(&cfg.opts.TokenFile, "prom-token-file", "", "file holding a bearer token for --prom")
	fs.StringVar(&cfg.opts.User, "prom-user", "", "basic auth user for --prom")
	fs.StringVar(&cfg.opts.PasswordFile, "prom-password-file", "", "file holding the basic auth password")
	fs.IntVar(&cfg.httpPort, "http-port", 0, "serve GET /screenshot and POST /variables/<name> on this loopback `port`; 0 disables")
	fs.BoolVar(&cfg.managed, "managed", false, "start kubectl proxy and a local VictoriaMetrics that scrapes pods (needs kubectl)")
	fs.StringVar(&cfg.mopts.KubeContext, "kube-context", "", "kubectl context for --managed (default: current context)")
	fs.Var((*stringsFlag)(&cfg.mopts.Namespaces), "namespace", "scrape only this `namespace` with --managed, repeatable (overrides scrape.namespaces)")
	fs.BoolVar(&cfg.mopts.InCluster, "in-cluster", false, "with --managed, run inside a pod: start only victoria-metrics and scrape pods directly with the pod's ServiceAccount (no kubectl)")
	fs.StringVar(&cfg.mopts.KubectlBin, "kubectl-bin", "", "kubectl binary for --managed (default: kubectl on PATH)")
	fs.StringVar(&cfg.mopts.VMBin, "vm-bin", "", "victoria-metrics binary for --managed (default: victoria-metrics on PATH)")
	fs.StringVar(&cfg.mopts.DataDir, "data-dir", "", "keep --managed data in this `dir` so history survives restarts (default: temp dir removed on exit)")
	fs.StringVar(&cfg.mopts.Retention, "retention", managed.DefaultRetention, "data retention for --managed, with a unit suffix (minimum 24h)")
	fs.StringVar(&cfg.mopts.ScrapeConfig, "scrape-config", "", "use this scrape config instead of generating one; ${KUBE_PROXY} is replaced by the proxy address")
	fs.StringVar(&cfg.mopts.LogFile, "log-file", "", "append kubectl and victoria-metrics output to this `file` (--managed)")
	fs.StringVar(&cfg.ssh.Listen, "ssh-listen", "", "serve the dashboard over SSH on this `address` (for example :2222) instead of the local terminal")
	fs.StringVar(&cfg.ssh.HostKeyPath, "ssh-host-key", defaultHostKey, "SSH host key `file`; an ed25519 key is generated when it does not exist")
	fs.StringVar(&cfg.ssh.AuthKeysPath, "ssh-authorized-keys", "", "OpenSSH authorized_keys `file` listing who may connect; re-read on every connection")
	fs.BoolVar(&cfg.ssh.AllowAnyKey, "ssh-allow-any-key", false, "let any public key connect (only for non-sensitive metrics)")
	fs.IntVar(&cfg.ssh.MaxSessions, "ssh-max-sessions", 20, "maximum number of concurrent SSH sessions")
	fs.BoolVar(&printScrape, "print-scrape-config", false, "print the in-cluster VictoriaMetrics scrape config for the dashboard and --namespace, then exit")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: omtui-dash [flags] dashboard.yaml\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if printScrape {
		return printScrapeConfig(fs, &cfg, stdout, stderr)
	}
	if cfg.promURL != "" && cfg.managed {
		_, _ = fmt.Fprintln(stderr, "omtui-dash: --prom and --managed are mutually exclusive")
		return 2
	}
	if !cfg.managed {
		var stray []string
		fs.Visit(func(f *flag.Flag) {
			if slices.Contains(managedOnlyFlags, f.Name) {
				stray = append(stray, "--"+f.Name)
			}
		})
		if len(stray) > 0 {
			_, _ = fmt.Fprintf(stderr, "omtui-dash: %s only apply with --managed\n", strings.Join(stray, ", "))
			return 2
		}
	}
	if cfg.mopts.InCluster {
		var bad []string
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "kube-context" || f.Name == "kubectl-bin" {
				bad = append(bad, "--"+f.Name)
			}
		})
		if len(bad) > 0 {
			_, _ = fmt.Fprintf(stderr, "omtui-dash: --in-cluster cannot be combined with %s: there is no kubectl\n", strings.Join(bad, ", "))
			return 2
		}
	}
	if code := validateSSH(fs, &cfg, stderr); code != 0 {
		return code
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	path := fs.Arg(0)
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return runCatalog(&cfg, path, stdout, stderr)
	}

	d, err := dashboard.Load(path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	for _, w := range d.Warnings {
		_, _ = fmt.Fprintf(stderr, "%s:%d: warning: %s\n", path, w.Line, w.Msg)
	}
	vals, err := d.Values(cfg.vars)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	be, err := newBackend(ctx, &cfg, d, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: %v\n", err)
		var se *managed.StartError
		if errors.As(err, &se) {
			return 1 // the backend failed to start, as opposed to bad usage
		}
		return 2
	}
	defer be.close()
	if cfg.check {
		if Check(ctx, d, vals, be.q, time.Now(), stdout) > 0 {
			return 1
		}
		return 0
	}
	if cfg.ssh.Listen != "" {
		return serveSSH(ctx, &cfg, func(ctx context.Context, sess ssh.Session) (tea.Model, []tea.ProgramOption) {
			// The renderer takes its colour profile from the client's TERM, not
			// the server's stdout. Wish sets the program's input and output itself.
			opts := dashtui.Options{
				Dashboard: d, Values: vals, Querier: be.q, Mode: be.mode, URL: be.url, NoSnapshot: true,
				Renderer: bubbletea.MakeRenderer(sess),
			}
			if be.managed != nil {
				opts.Backend = be.managed // shared: every session sees health and can restart
			}
			return dashtui.NewSession(ctx, opts), []tea.ProgramOption{tea.WithAltScreen()}
		}, stderr)
	}
	opts := dashtui.Options{Dashboard: d, Values: vals, Querier: be.q, Mode: be.mode, URL: be.url, HTTPPort: cfg.httpPort}
	if be.managed != nil {
		opts.Backend = be.managed
		opts.KubeContext = be.managed.KubeContext()
	}
	if err := dashtui.Run(ctx, opts); err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: %v\n", err)
		return 1
	}
	return 0
}

// printScrapeConfig prints the in-cluster scrape config, the source for the
// sidecar's ConfigMap. It needs no backend or cluster, so only the dashboard
// path (optional) and --namespace may accompany it.
func printScrapeConfig(fs *flag.FlagSet, cfg *config, stdout, stderr io.Writer) int {
	var bad []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "print-scrape-config" && f.Name != "namespace" {
			bad = append(bad, "--"+f.Name)
		}
	})
	if len(bad) > 0 {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: --print-scrape-config cannot be combined with %s\n", strings.Join(bad, ", "))
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	var scrape *dashboard.Scrape
	if fs.NArg() == 1 {
		path := fs.Arg(0)
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			var code int
			if scrape, code = catalogScrape(path, stderr); code != 0 {
				return code
			}
		} else {
			d, err := dashboard.Load(path)
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				return 1
			}
			for _, w := range d.Warnings {
				_, _ = fmt.Fprintf(stderr, "%s:%d: warning: %s\n", path, w.Line, w.Msg)
			}
			scrape = d.Scrape
		}
	}
	out, err := managed.GenerateScrapeConfig(managed.ScrapeParams{Scrape: scrape, Namespaces: cfg.mopts.Namespaces, InCluster: true})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: %v\n", err)
		return 1
	}
	_, _ = stdout.Write(out)
	return 0
}

// catalogScrape returns the scrape section of the one dashboard in dir that has
// one, or nil when none does. Several is an error, since the scrape config is
// shared and there is no telling which dashboard is meant.
func catalogScrape(dir string, stderr io.Writer) (*dashboard.Scrape, int) {
	cat, err := dashboard.LoadCatalog(dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return nil, 1
	}
	var with []string
	var scrape *dashboard.Scrape
	for _, name := range cat.Names() {
		if d, _ := cat.Get(name); d.Scrape != nil {
			with = append(with, name)
			scrape = d.Scrape
		}
	}
	if len(with) > 1 {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: only one dashboard in %s may have a scrape section, found %s\n", dir, strings.Join(with, ", "))
		return nil, 1
	}
	return scrape, 0
}

// validateSSH checks the --ssh-* flags and returns the exit code to stop with,
// or 0 to go on.
func validateSSH(fs *flag.FlagSet, cfg *config, stderr io.Writer) int {
	if cfg.ssh.Listen == "" {
		var stray []string
		fs.Visit(func(f *flag.Flag) {
			if slices.Contains(sshOnlyFlags, f.Name) {
				stray = append(stray, "--"+f.Name)
			}
		})
		if len(stray) > 0 {
			_, _ = fmt.Fprintf(stderr, "omtui-dash: %s only apply with --ssh-listen\n", strings.Join(stray, ", "))
			return 2
		}
		return 0
	}
	var bad []string
	// Plain --managed would use the server's kubeconfig for everyone who
	// connects; in-cluster uses the pod's own ServiceAccount.
	if cfg.managed && !cfg.mopts.InCluster {
		bad = append(bad, "--managed (without --in-cluster)")
	}
	if cfg.check {
		bad = append(bad, "--check")
	}
	if cfg.httpPort != 0 {
		bad = append(bad, "--http-port")
	}
	if len(bad) > 0 {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: --ssh-listen cannot be combined with %s\n", strings.Join(bad, ", "))
		return 2
	}
	if cfg.ssh.AuthKeysPath == "" && !cfg.ssh.AllowAnyKey {
		_, _ = fmt.Fprintln(stderr, "omtui-dash: --ssh-listen needs --ssh-authorized-keys or --ssh-allow-any-key")
		return 2
	}
	if cfg.ssh.MaxSessions < 1 {
		_, _ = fmt.Fprintln(stderr, "omtui-dash: --ssh-max-sessions must be at least 1")
		return 2
	}
	return 0
}

// runCatalog is Run for a directory of dashboards: it checks them all, or
// serves them over SSH, where each connection picks one. The directory needs
// --prom or --managed and takes no --var, because the dashboards have
// different variables.
func runCatalog(cfg *config, dir string, stdout, stderr io.Writer) int {
	switch {
	case cfg.promURL == "" && !cfg.managed:
		_, _ = fmt.Fprintln(stderr, "omtui-dash: a dashboard directory needs --prom or --managed")
		return 2
	case len(cfg.vars) > 0:
		_, _ = fmt.Fprintln(stderr, "omtui-dash: --var does not apply to a dashboard directory")
		return 2
	case !cfg.check && cfg.ssh.Listen == "":
		_, _ = fmt.Fprintln(stderr, "omtui-dash: a dashboard directory needs --ssh-listen or --check")
		return 2
	}
	cat, err := dashboard.LoadCatalog(dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	for _, w := range cat.Skipped() {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: warning: %s\n", w)
	}
	for _, name := range cat.Names() {
		d, _ := cat.Get(name)
		for _, w := range d.Warnings {
			_, _ = fmt.Fprintf(stderr, "%s:%d: warning: %s\n", name, w.Line, w.Msg)
		}
		if d.Datasource.URL != "" {
			_, _ = fmt.Fprintf(stderr, "omtui-dash: warning: %s: datasource.url is ignored, using --prom\n", name)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// An external backend does not depend on the dashboards; a managed one
	// scrapes what the one dashboard with a scrape section asks for.
	var scrape *dashboard.Scrape
	if cfg.managed {
		var code int
		if scrape, code = catalogScrape(dir, stderr); code != 0 {
			return code
		}
	}
	be, err := newBackend(ctx, cfg, &dashboard.Dashboard{Scrape: scrape}, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: %v\n", err)
		var se *managed.StartError
		if errors.As(err, &se) {
			return 1
		}
		return 2
	}
	defer be.close()
	if cfg.check {
		failed := 0
		for _, name := range cat.Names() {
			d, _ := cat.Get(name)
			vals, err := d.Values(nil)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "omtui-dash: %s: %v\n", name, err)
				return 2
			}
			_, _ = fmt.Fprintf(stdout, "== %s\n", name)
			failed += Check(ctx, d, vals, be.q, time.Now(), stdout)
		}
		if failed > 0 {
			return 1
		}
		return 0
	}
	base := dashtui.Options{Querier: be.q, Mode: be.mode, URL: be.url, NoSnapshot: true}
	if be.managed != nil {
		base.Backend = be.managed
	}
	return serveSSH(ctx, cfg, sshserve.CatalogModel(cat, base), stderr)
}

// serveSSH serves one dashboard session per SSH connection, all sharing the
// backend's Querier, until ctx is cancelled.
func serveSSH(ctx context.Context, cfg *config, newModel sshserve.NewModel, stderr io.Writer) int {
	if cfg.ssh.AllowAnyKey {
		_, _ = fmt.Fprintln(stderr, "omtui-dash: warning: --ssh-allow-any-key lets anyone who can reach the port view the dashboard")
	}
	cfg.ssh.OnListen = func(addr net.Addr, fingerprint string) {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: serving SSH on %s, host key %s\n", addr, fingerprint)
	}
	if err := sshserve.Serve(ctx, cfg.ssh, newModel); err != nil {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: %v\n", err)
		return 1
	}
	return 0
}

// backend is the chosen data source: its Querier, a label for the status line,
// the managed supervisor (nil for an external backend) and its release.
type backend struct {
	q       promapi.Querier
	mode    string
	url     string // external backend only
	managed *managed.Backend
	close   func()
}

// newBackend picks the backend (see managed.SelectMode) and returns it.
// Startup failures of the managed backend are reported before anything else
// happens.
func newBackend(ctx context.Context, cfg *config, d *dashboard.Dashboard, stderr io.Writer) (*backend, error) {
	mode, url, err := managed.SelectMode(cfg.promURL, cfg.managed, d.Datasource.URL)
	if err != nil {
		return nil, err
	}
	if mode == managed.ModeExternal {
		opts := cfg.opts
		opts.URL = url
		q, err := promapi.New(opts)
		if err != nil {
			return nil, err
		}
		return &backend{q: q, mode: "external " + url, url: url, close: func() {}}, nil
	}
	mopts := cfg.mopts
	mopts.Scrape = d.Scrape
	mopts.Interactive = stderr
	b, err := managed.Start(ctx, mopts)
	if err != nil {
		return nil, err
	}
	for _, w := range b.Warnings() {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: warning: %s\n", w)
	}
	ctxName := cfg.mopts.KubeContext
	switch {
	case cfg.mopts.InCluster:
		ctxName = "in-cluster"
	case ctxName == "":
		ctxName = "current context"
	}
	return &backend{q: b.Querier(), mode: "managed " + ctxName, managed: b, close: func() { _ = b.Close() }}, nil
}

const (
	checkWidth   = 120
	checkMinStep = 15 * time.Second
	checkTimeout = 30 * time.Second
)

// Check runs every panel query once, printing one status line per panel, and
// returns the number of panels that failed.
func Check(ctx context.Context, d *dashboard.Dashboard, vals map[string]string, q promapi.Querier, now time.Time, w io.Writer) int {
	rng := max(d.Spark, d.Windows[len(d.Windows)-1].Offset)
	start := now.Add(-rng)
	step := promapi.Step(start, now, checkWidth, checkMinStep)

	failed := 0
	for _, p := range d.Panels {
		query, err := dashboard.Substitute(p.Query, vals)
		var series int
		if err == nil {
			qctx, cancel := context.WithTimeout(ctx, checkTimeout)
			var res []promapi.Series
			res, err = q.QueryRange(qctx, query, start, now, step)
			cancel()
			series = len(res)
		}
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(w, "FAIL  %s: %v\n", p.Title, err)
			continue
		}
		note := ""
		if series == 0 {
			note = " (no data)"
		}
		_, _ = fmt.Fprintf(w, "ok    %s: %d rows%s\n", p.Title, series, note)
	}
	if failed > 0 {
		_, _ = fmt.Fprintf(w, "%d of %d panels failed\n", failed, len(d.Panels))
	}
	return failed
}
