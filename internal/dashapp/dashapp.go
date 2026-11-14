// Package dashapp is the omtui-dash command: flag handling, backend
// selection (external or managed), the --check mode and the TUI launch.
package dashapp

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/dashtui"
	"github.com/michaelvl/openmetrics-tui/internal/managed"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
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

// managedOnlyFlags are meaningless without --managed.
var managedOnlyFlags = []string{
	"kube-context", "namespace", "kubectl-bin", "vm-bin",
	"data-dir", "retention", "scrape-config", "log-file",
}

type config struct {
	check   bool
	vars    varFlags
	promURL string
	opts    promapi.Options
	managed bool
	mopts   managed.Options
}

// Main runs the command and exits the process.
func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run is Main without the exit, returning the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("omtui-dash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config{vars: varFlags{}}
	fs.BoolVar(&cfg.check, "check", false, "validate the dashboard and run every query once, then exit")
	fs.Var(cfg.vars, "var", "set a variable, `name=value` (repeatable)")
	fs.StringVar(&cfg.promURL, "prom", "", "Prometheus-compatible API `URL` (overrides datasource.url)")
	fs.StringVar(&cfg.opts.TokenFile, "prom-token-file", "", "file holding a bearer token for --prom")
	fs.StringVar(&cfg.opts.User, "prom-user", "", "basic auth user for --prom")
	fs.StringVar(&cfg.opts.PasswordFile, "prom-password-file", "", "file holding the basic auth password")
	fs.BoolVar(&cfg.managed, "managed", false, "start kubectl proxy and a local VictoriaMetrics that scrapes pods (needs kubectl)")
	fs.StringVar(&cfg.mopts.KubeContext, "kube-context", "", "kubectl context for --managed (default: current context)")
	fs.StringVar(&cfg.mopts.Namespace, "namespace", "", "scrape only this namespace with --managed (overrides scrape.namespaces)")
	fs.StringVar(&cfg.mopts.KubectlBin, "kubectl-bin", "", "kubectl binary for --managed (default: kubectl on PATH)")
	fs.StringVar(&cfg.mopts.VMBin, "vm-bin", "", "victoria-metrics binary for --managed (default: victoria-metrics on PATH)")
	fs.StringVar(&cfg.mopts.DataDir, "data-dir", "", "keep --managed data in this `dir` so history survives restarts (default: temp dir removed on exit)")
	fs.StringVar(&cfg.mopts.Retention, "retention", managed.DefaultRetention, "data retention for --managed, with a unit suffix (minimum 24h)")
	fs.StringVar(&cfg.mopts.ScrapeConfig, "scrape-config", "", "use this scrape config instead of generating one; ${KUBE_PROXY} is replaced by the proxy address")
	fs.StringVar(&cfg.mopts.LogFile, "log-file", "", "append kubectl and victoria-metrics output to this `file` (--managed)")
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
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	path := fs.Arg(0)

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
	opts := dashtui.Options{Dashboard: d, Values: vals, Querier: be.q, Mode: be.mode}
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

// backend is the chosen data source: its Querier, a label for the status line,
// the managed supervisor (nil for an external backend) and its release.
type backend struct {
	q       promapi.Querier
	mode    string
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
		return &backend{q: q, mode: "external " + url, close: func() {}}, nil
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
	if ctxName == "" {
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
