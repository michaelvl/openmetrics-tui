package managed

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/promapi"
)

// These tests run a real victoria-metrics and are skipped when it is not on
// PATH (the Nix build sandbox, for example). They need no cluster: the
// Kubernetes API is the fake from fakebin.

func needVM(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("victoria-metrics")
	if err != nil {
		t.Skip("victoria-metrics not on PATH")
	}
	return p
}

// poll runs fn until it returns true or 40s pass.
func poll(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func instant(q promapi.Querier, expr string) []promapi.Series {
	res, err := q.Instant(context.Background(), expr, time.Now())
	if err != nil {
		return nil
	}
	return res
}

func TestIntegrationStaticTargetMockServer(t *testing.T) {
	vmBin := needVM(t)
	dir := t.TempDir()
	mock := filepath.Join(dir, "mock-server")
	if out, err := exec.Command("go", "build", "-o", mock, "../../cmd/mock-server").CombinedOutput(); err != nil {
		t.Fatalf("build mock-server: %v\n%s", err, out)
	}
	addr, err := freeAddr()
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(addr)
	lb := NewLogBuffer(100, nil)
	srv, err := startChild("mock-server", mock, []string{"-port", port}, nil, lb, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.stop(time.Second)

	cfg := filepath.Join(dir, "static.yml")
	body := fmt.Sprintf("scrape_configs:\n  - job_name: mock\n    scrape_interval: 1s\n    static_configs:\n      - targets: ['%s']\n", addr)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{VMBin: vmBin, ScrapeConfig: cfg, skipProxy: true}
	b, err := Start(ctx(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if len(b.Warnings()) != 0 {
		t.Logf("warnings: %v", b.Warnings())
	}

	poll(t, "mock-server samples", func() bool {
		return len(instant(b, `http_requests_total{job="mock"}`)) > 0
	})
	up := instant(b, `up{job="mock"}`)
	if len(up) != 1 || up[0].Points[0].V != 1 {
		t.Errorf("up: %+v", up)
	}
	// Range query, as the dashboard does.
	res, err := b.QueryRange(ctx(t), `sum(http_requests_total)`, time.Now().Add(-time.Minute), time.Now(), 5*time.Second)
	if err != nil || len(res) != 1 || len(res[0].Points) == 0 {
		t.Errorf("range: %v %v", res, err)
	}
}

// startSelfScrapingVM runs victoria-metrics directly, scraping its own
// /metrics every second, with extra flags. It returns a client.
func startSelfScrapingVM(t *testing.T, vmBin string, extra ...string) *promapi.Client {
	t.Helper()
	addr, err := freeAddr()
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "self.yml")
	body := fmt.Sprintf("scrape_configs:\n  - job_name: self\n    scrape_interval: 1s\n    static_configs:\n      - targets: ['%s']\n", addr)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-promscrape.config=" + cfg, "-storageDataPath=" + t.TempDir(),
		"-httpListenAddr=" + addr, "-retentionPeriod=24h"}, extra...)
	c, err := startChild("vm", vmBin, args, nil, NewLogBuffer(100, nil), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.stop(5 * time.Second) })
	if err := c.waitReady(ctx(t), 30*time.Second, func(x context.Context) error { return httpOK(x, "http://"+addr+"/health") }); err != nil {
		t.Fatal(err)
	}
	cl, err := promapi.New(promapi.Options{URL: "http://" + addr})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// TestVMLatencyOffset records the finding behind passing -search.latencyOffset=0:
// with the default (30s) the newest samples are not visible to a query at
// "now"; with 0 they are.
func TestVMLatencyOffset(t *testing.T) {
	vmBin := needVM(t)
	t.Run("default", func(t *testing.T) {
		cl := startSelfScrapingVM(t, vmBin)
		time.Sleep(6 * time.Second) // several scrapes have landed
		got := len(instant(cl, `up`))
		t.Logf("default latencyOffset: %d series visible 6s after startup", got)
		if got != 0 {
			t.Errorf("the default offset no longer hides fresh samples; revisit -search.latencyOffset=0")
		}
	})
	t.Run("zero", func(t *testing.T) {
		cl := startSelfScrapingVM(t, vmBin, "-search.latencyOffset=0")
		poll(t, "fresh sample visible at now", func() bool { return len(instant(cl, `up`)) > 0 })
	})
}

func TestIntegrationFakeKubernetes(t *testing.T) {
	vmBin := needVM(t)
	type want struct {
		instances []string // sorted instance label values
		pathHas   map[string]string
	}
	cases := []struct {
		name   string
		scrape dashboard.Scrape
		want   want
	}{
		{
			name: "annotation",
			scrape: dashboard.Scrape{Namespaces: []string{"prod"}, Interval: time.Second,
				Annotation: "prometheus.io/scrape", Port: "prometheus.io/port", Path: "prometheus.io/path"},
			want: want{
				// c, g: not annotated; d: Pending; h: no port annotation.
				instances: []string{"pod-a", "pod-b", "pod-e", "pod-f"},
				pathHas: map[string]string{
					"pod-a": "/api/v1/namespaces/prod/pods/pod-a:9100/proxy/custom",
					"pod-b": "/api/v1/namespaces/prod/pods/pod-b:9101/proxy/metrics",
					"pod-e": "/api/v1/namespaces/prod/pods/pod-e:9103/proxy/metrics",
					"pod-f": "/api/v1/namespaces/prod/pods/pod-f:9105/proxy/metrics",
				},
			},
		},
		{
			name: "selector declared ports",
			scrape: dashboard.Scrape{Namespaces: []string{"prod"}, Interval: time.Second,
				Selector: "app=foo"},
			want: want{
				// g declares no port and is skipped; a, b, c are scraped on their ports.
				instances: []string{"pod-a:9100", "pod-b:9101", "pod-c:8080"},
				pathHas: map[string]string{
					"pod-a:9100": "/api/v1/namespaces/prod/pods/pod-a:9100/proxy/metrics",
					"pod-c:8080": "/api/v1/namespaces/prod/pods/pod-c:8080/proxy/metrics",
				},
			},
		},
		{
			name: "selector fixed port",
			scrape: dashboard.Scrape{Namespaces: []string{"prod"}, Interval: time.Second,
				Selector: "app=foo", Port: "7000", Path: "/stats"},
			want: want{
				// VictoriaMetrics makes no target for a pod without declared
				// container ports, so pod-g is skipped even with a fixed port.
				instances: []string{"pod-a", "pod-b", "pod-c"},
				pathHas: map[string]string{
					"pod-a": "/api/v1/namespaces/prod/pods/pod-a:7000/proxy/stats",
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := fakeOptions(t)
			t.Setenv("FAKE_MODE_KUBECTL", "kubeapi")
			fake := opts.wrap
			opts.wrap = func(tool string, args []string) (string, []string) {
				if tool == "kubectl" {
					return fake(tool, args)
				}
				return vmBin, args
			}
			opts.Scrape = &tc.scrape
			opts.StartupTimeout = 30 * time.Second
			b, err := Start(ctx(t), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = b.Close() }()
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("child logs:\n%s", strings.Join(b.Logs(), "\n"))
				}
			})

			var res []promapi.Series
			poll(t, "all expected pods scraped", func() bool {
				res = instant(b, "probe_hits")
				return len(res) >= len(tc.want.instances)
			})
			time.Sleep(3 * time.Second) // give unexpected (extra or duplicate) targets time to show up
			res = instant(b, "probe_hits")
			var got []string
			for _, s := range res {
				got = append(got, s.Labels["instance"])
				if s.Labels["namespace"] != "prod" || s.Labels["pod"] == "" || s.Labels["job"] != "pods-via-proxy" {
					t.Errorf("labels %v", s.Labels)
				}
				for k := range s.Labels {
					if k != "__name__" && strings.HasPrefix(k, "__") || k == "container" {
						t.Errorf("unexpected label %q in %v", k, s.Labels)
					}
				}
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want.instances, ",") {
				t.Errorf("instances %v, want %v", got, tc.want.instances)
			}
			for inst, path := range tc.want.pathHas {
				pr := instant(b, fmt.Sprintf(`probe_path{instance=%q,path=%q}`, inst, path))
				if len(pr) != 1 {
					t.Errorf("no probe_path for %s = %s (scrape path wrong)", inst, path)
				}
			}
		})
	}
}
