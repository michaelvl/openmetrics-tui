package managed

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestScrapeConfigGolden(t *testing.T) {
	const proxy = "127.0.0.1:8001"
	cases := map[string]ScrapeParams{
		// Appendix B of the plan.
		"annotation": {ProxyAddr: proxy, Scrape: &dashboard.Scrape{
			Namespaces: []string{"prod"}, Interval: 15 * time.Second,
			Annotation: "prometheus.io/scrape", Port: "prometheus.io/port", Path: "prometheus.io/path",
		}},
		// Annotation mode with default annotation names and a custom domain.
		"annotation-defaults": {ProxyAddr: proxy, Scrape: &dashboard.Scrape{
			Namespaces: []string{"a", "b"}, Interval: 30 * time.Second, Annotation: "example.com/scrape",
		}},
		"selector": {ProxyAddr: proxy, Scrape: &dashboard.Scrape{
			Namespaces: []string{"prod"}, Interval: 15 * time.Second, Selector: "app=foo,tier in (a,b)",
		}},
		"selector-port-path": {ProxyAddr: proxy, Scrape: &dashboard.Scrape{
			Namespaces: []string{"prod"}, Interval: 15 * time.Second, Selector: "app=foo", Port: "8080", Path: "/stats/prom",
		}},
		// No scrape section: every Running pod with a declared port, all namespaces.
		"default": {ProxyAddr: proxy},
		// --namespace overrides scrape.namespaces.
		"namespace-override": {ProxyAddr: proxy, Namespaces: []string{"override"}, Scrape: &dashboard.Scrape{
			Namespaces: []string{"prod"}, Interval: 15 * time.Second, Selector: "app=foo",
		}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := GenerateScrapeConfig(p)
			if err != nil {
				t.Fatal(err)
			}
			var parsed any
			if err := yaml.Unmarshal(got, &parsed); err != nil {
				t.Fatalf("not valid YAML: %v", err)
			}
			path := filepath.Join("testdata", "scrape", name+".golden")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("mismatch with %s (rerun with -update):\n%s", path, got)
			}
		})
	}
}

func TestMetaAnnotation(t *testing.T) {
	for in, want := range map[string]string{
		"prometheus.io/port":    "__meta_kubernetes_pod_annotation_prometheus_io_port",
		"example.com/my-scrape": "__meta_kubernetes_pod_annotation_example_com_my_scrape",
		"already_ok":            "__meta_kubernetes_pod_annotation_already_ok",
	} {
		if got := metaAnnotation(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestGenerateNeedsProxy(t *testing.T) {
	if _, err := GenerateScrapeConfig(ScrapeParams{}); err == nil {
		t.Error("want error")
	}
}
