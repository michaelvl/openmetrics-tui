// Package dashapp is the omtui-dash command: flag handling, backend
// selection and the --check mode. The TUI arrives in Phase 5.
package dashapp

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
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

// managedFlags are accepted so scripts and help text are stable, but the
// managed backend is not implemented until Phase 4.
var managedFlags = []string{
	"managed", "kube-context", "namespace", "kubectl-bin", "vm-bin",
	"data-dir", "retention", "scrape-config", "log-file",
}

type config struct {
	check   bool
	vars    varFlags
	promURL string
	opts    promapi.Options
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
	fs.Bool("managed", false, "(not implemented yet) start kubectl proxy and a local VictoriaMetrics")
	fs.String("kube-context", "", "(not implemented yet) kubectl context for --managed")
	fs.String("namespace", "", "(not implemented yet) namespace for --managed")
	fs.String("kubectl-bin", "", "(not implemented yet) kubectl binary for --managed")
	fs.String("vm-bin", "", "(not implemented yet) victoria-metrics binary for --managed")
	fs.String("data-dir", "", "(not implemented yet) data directory for --managed")
	fs.String("retention", "", "(not implemented yet) retention for --managed")
	fs.String("scrape-config", "", "(not implemented yet) scrape config override for --managed")
	fs.String("log-file", "", "(not implemented yet) log file for --managed")
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

	var unimplemented []string
	fs.Visit(func(f *flag.Flag) {
		for _, m := range managedFlags {
			if f.Name == m {
				unimplemented = append(unimplemented, "--"+m)
			}
		}
	})
	if len(unimplemented) > 0 {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: managed mode is not implemented yet (Phase 4); cannot use %s\n", strings.Join(unimplemented, ", "))
		return 2
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

	if !cfg.check {
		_, _ = fmt.Fprintln(stderr, "omtui-dash: TUI not implemented yet (Phase 5); use --check to validate and query the dashboard")
		return 1
	}

	q, err := newQuerier(&cfg, d)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "omtui-dash: %v\n", err)
		return 2
	}
	if Check(context.Background(), d, vals, q, time.Now(), stdout) > 0 {
		return 1
	}
	return 0
}

// newQuerier picks the backend: --prom, else datasource.url.
func newQuerier(cfg *config, d *dashboard.Dashboard) (promapi.Querier, error) {
	opts := cfg.opts
	opts.URL = cfg.promURL
	if opts.URL == "" {
		opts.URL = d.Datasource.URL
	}
	if opts.URL == "" {
		return nil, errors.New("no backend: pass --prom URL or set datasource.url in the dashboard")
	}
	return promapi.New(opts)
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
