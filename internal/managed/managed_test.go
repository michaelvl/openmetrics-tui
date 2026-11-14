package managed

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
)

func TestStartQueryClose(t *testing.T) {
	opts := fakeOptions(t)
	af := argsFile(t)
	opts.KubeContext = "kind-x"
	opts.Scrape = &dashboard.Scrape{Namespaces: []string{"prod", "dev"}, Interval: 15 * time.Second, Annotation: "prometheus.io/scrape"}
	b, err := Start(ctx(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	if h := b.Health(); len(h) != 2 || !h[0].Running || !h[1].Running {
		t.Errorf("health %+v", h)
	}
	if !strings.HasPrefix(b.URL(), "http://127.0.0.1:") || !strings.Contains(b.Describe(), "kind-x") {
		t.Errorf("url %q describe %q", b.URL(), b.Describe())
	}
	res, err := b.Querier().QueryRange(ctx(t), "up", time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err != nil || len(res) != 1 || res[0].Labels["pod"] != "p" {
		t.Fatalf("query: %v %v", res, err)
	}
	if len(leftovers(t)) != 1 {
		t.Errorf("want one temp dir while running, got %v", leftovers(t))
	}
	proxy, vm := b.proxy, b.vm
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if !proxy.exited() || !vm.exited() {
		t.Error("children still running after Close")
	}
	if l := leftovers(t); len(l) != 0 {
		t.Errorf("temp dir not removed: %v", l)
	}
	if err := b.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	raw, _ := os.ReadFile(af)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var got []string
	for _, l := range lines {
		got = append(got, strings.Join(strings.Fields(l)[:2], " "))
	}
	order := []string{
		"kubectl version", "victoria-metrics -version",
		"kubectl auth", "kubectl auth", "kubectl auth", "kubectl auth", // 2 checks x 2 namespaces
		"kubectl proxy", "victoria-metrics -promscrape.config=" + "",
	}
	for i, want := range order[:7] {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("start order[%d] = %q, want %q (all: %q)", i, got[i], want, got)
		}
	}
	all := string(raw)
	for _, want := range []string{
		"auth can-i get pods --subresource=proxy -n prod --context=kind-x",
		"auth can-i list pods -n dev --context=kind-x",
		"proxy --port=0 --address=127.0.0.1 --context=kind-x",
		"-retentionPeriod=24h", "-search.latencyOffset=0", "-storageDataPath=", "-httpListenAddr=127.0.0.1:",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("args missing %q:\n%s", want, all)
		}
	}
	if !strings.HasPrefix(got[7], "victoria-metrics -promscrape.config=") {
		t.Errorf("vm line %q", got[7])
	}
}

func TestAllNamespacesPreflight(t *testing.T) {
	opts := fakeOptions(t)
	af := argsFile(t)
	b, err := Start(ctx(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	raw, _ := os.ReadFile(af)
	if !strings.Contains(string(raw), "auth can-i list pods --all-namespaces") {
		t.Errorf("want all-namespaces check:\n%s", raw)
	}
}

func TestPreflightRBAC(t *testing.T) {
	opts := fakeOptions(t)
	t.Setenv("FAKE_MODE_KUBECTL", "norbac")
	_, err := Start(ctx(t), opts)
	var se *StartError
	if !errors.As(err, &se) {
		t.Fatalf("want StartError, got %v", err)
	}
	if !strings.Contains(se.Step, "kubectl auth can-i get pods --subresource=proxy") || !strings.Contains(se.Error(), "pods/proxy") {
		t.Errorf("error %q", se)
	}
	if l := leftovers(t); len(l) != 0 {
		t.Errorf("temp dir leaked: %v", l)
	}
}

func TestMissingBinaries(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	_, err := Start(ctx(t), Options{KubectlBin: "/nonexistent/kubectl"})
	if err == nil || !strings.Contains(err.Error(), "install kubectl") || !strings.Contains(err.Error(), "kubectl version --client") {
		t.Errorf("kubectl: %v", err)
	}
	fake, _ := exec.LookPath("true")
	_, err = Start(ctx(t), Options{KubectlBin: fake, VMBin: "/nonexistent/victoria-metrics"})
	if err == nil || !strings.Contains(err.Error(), "victoria-metrics -version") || !strings.Contains(err.Error(), "--vm-bin") {
		t.Errorf("vm: %v", err)
	}
	if l := leftovers(t); len(l) != 0 {
		t.Errorf("temp dir leaked: %v", l)
	}
}

func TestReadinessTimeouts(t *testing.T) {
	for _, tc := range []struct{ env, step string }{
		{"FAKE_MODE_KUBECTL", "start kubectl proxy"},
		{"FAKE_MODE_VM", "start victoria-metrics"},
	} {
		opts := fakeOptions(t)
		opts.StartupTimeout = 600 * time.Millisecond
		t.Setenv(tc.env, "hang")
		t0 := time.Now()
		_, err := Start(ctx(t), opts)
		var se *StartError
		if !errors.As(err, &se) || se.Step != tc.step || !strings.Contains(err.Error(), "not ready after") {
			t.Errorf("%s: %v", tc.env, err)
		}
		if time.Since(t0) > 10*time.Second {
			t.Errorf("%s: took %s", tc.env, time.Since(t0))
		}
		if l := leftovers(t); len(l) != 0 {
			t.Errorf("%s: temp dir leaked: %v", tc.env, l)
		}
		t.Setenv(tc.env, "")
	}
}

func TestStartupCrashHasOutputTail(t *testing.T) {
	opts := fakeOptions(t)
	t.Setenv("FAKE_MODE_VM", "crash-start")
	_, err := Start(ctx(t), opts)
	var se *StartError
	if !errors.As(err, &se) || se.Step != "start victoria-metrics" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "exited before it was ready") || !strings.Contains(err.Error(), "fake: failed to start: boom") {
		t.Errorf("error lacks reason or tail: %v", err)
	}
}

func TestPortClashRetry(t *testing.T) {
	opts := fakeOptions(t)
	t.Setenv("FAKE_MODE_VM", "clash-once")
	t.Setenv("FAKE_STATE_FILE", t.TempDir()+"/state")
	af := argsFile(t)
	b, err := Start(ctx(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	raw, _ := os.ReadFile(af)
	if n := strings.Count(string(raw), "victoria-metrics -promscrape"); n != 2 {
		t.Errorf("want 2 VM starts, got %d:\n%s", n, raw)
	}
}

func TestCrashDetectionAndRestart(t *testing.T) {
	opts := fakeOptions(t)
	t.Setenv("FAKE_MODE_VM", "crash")
	b, err := Start(ctx(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	select {
	case ev := <-b.Events():
		if ev.Child != "victoria-metrics" || ev.Err == nil {
			t.Errorf("event %+v", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit event")
	}
	var vm ChildStatus
	for _, h := range b.Health() {
		if h.Name == "victoria-metrics" {
			vm = h
		}
	}
	if vm.Running || vm.Err == nil {
		t.Errorf("health %+v", vm)
	}
	if !strings.Contains(strings.Join(b.Logs(), "\n"), "fake: crashing") {
		t.Errorf("logs: %q", b.Logs())
	}

	t.Setenv("FAKE_MODE_VM", "")
	if err := b.Restart(ctx(t)); err != nil {
		t.Fatal(err)
	}
	for _, h := range b.Health() {
		if !h.Running {
			t.Errorf("after restart: %+v", h)
		}
	}
	if _, err := b.Querier().Instant(ctx(t), "up", time.Now()); err != nil {
		t.Errorf("query after restart: %v", err)
	}
}

func TestSigkillEscalation(t *testing.T) {
	opts := fakeOptions(t)
	opts.ShutdownGrace = 300 * time.Millisecond
	t.Setenv("FAKE_MODE_KUBECTL", "ignore-term")
	b, err := Start(ctx(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	proxy := b.proxy
	t0 := time.Now()
	_ = b.Close()
	if !proxy.exited() {
		t.Fatal("proxy survived Close")
	}
	if time.Since(t0) < 300*time.Millisecond {
		t.Errorf("returned before the grace period: %s", time.Since(t0))
	}
	if !strings.Contains(strings.Join(b.Logs(), "\n"), "sending SIGKILL") {
		t.Errorf("no SIGKILL in logs: %q", b.Logs())
	}
}

func TestLogFileAndRing(t *testing.T) {
	opts := fakeOptions(t)
	opts.LogFile = t.TempDir() + "/children.log"
	t.Setenv("FAKE_MODE_VM", "crash-start")
	_, _ = Start(ctx(t), opts)
	raw, err := os.ReadFile(opts.LogFile)
	if err != nil || !strings.Contains(string(raw), "[victoria-metrics] fake: failed to start: boom") {
		t.Errorf("log file %q: %v", raw, err)
	}

	lb := NewLogBuffer(3, nil)
	for _, l := range []string{"1", "2", "3", "4", "5"} {
		lb.Add(l)
	}
	if got := strings.Join(lb.Lines(), ","); got != "3,4,5" {
		t.Errorf("ring %q", got)
	}
	if got := strings.Join(lb.Tail(2), ","); got != "4,5" {
		t.Errorf("tail %q", got)
	}
}

func TestSelectMode(t *testing.T) {
	for _, tc := range []struct {
		prom    string
		managed bool
		ds      string
		mode    Mode
		url     string
		err     string
	}{
		{"http://p", false, "http://d", ModeExternal, "http://p", ""},
		{"", true, "http://d", ModeManaged, "", ""},
		{"", false, "http://d", ModeExternal, "http://d", ""},
		{"http://p", true, "", 0, "", "mutually exclusive"},
		{"", false, "", 0, "", "--prom URL"},
	} {
		mode, url, err := SelectMode(tc.prom, tc.managed, tc.ds)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%+v: err %v", tc, err)
			}
			continue
		}
		if err != nil || mode != tc.mode || url != tc.url {
			t.Errorf("%+v: got %v %q %v", tc, mode, url, err)
		}
	}
	_, _, err := SelectMode("", false, "")
	if !strings.Contains(err.Error(), "--managed") || !strings.Contains(err.Error(), "datasource.url") {
		t.Errorf("error should list all options: %v", err)
	}
}

func TestValidateRetention(t *testing.T) {
	for s, ok := range map[string]bool{
		"24h": true, "1d": true, "7d": true, "2w": true, "1M": true, "1y": true,
		"23h": false, "1h": false, "90m": false, "1": false, "": false, "24": false, "x": false, "1.5d": false,
	} {
		if err := ValidateRetention(s); (err == nil) != ok {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for in, warn := range map[string]bool{
		"1.156.0\n": false, // devbox and Nix print the bare version
		"victoria-metrics-20250101-120000-tags-v1.110.0-0-gabcdef": false,
		"1.100.0":  false,
		"v1.99.9":  true,
		"0.9.0":    true,
		"2.0.0":    false,
		"nonsense": true,
	} {
		if got := CheckVersion(in) != ""; got != warn {
			t.Errorf("%q: warn=%v", in, got)
		}
	}
}

func TestVersionWarningCollected(t *testing.T) {
	// the fake reports 1.156.0, so there is no warning
	opts := fakeOptions(t)
	b, err := Start(ctx(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if len(b.Warnings()) != 0 {
		t.Errorf("warnings %v", b.Warnings())
	}
}

func TestScrapeConfigOverride(t *testing.T) {
	opts := fakeOptions(t)
	cfg := t.TempDir() + "/user.yml"
	_ = os.WriteFile(cfg, []byte("api_server: http://${KUBE_PROXY}\nagain: ${KUBE_PROXY}\n"), 0o600)
	opts.ScrapeConfig = cfg
	b, err := Start(ctx(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	raw, err := os.ReadFile(b.tmpDir + "/scrape.yml")
	if err != nil || strings.Contains(string(raw), "${KUBE_PROXY}") || !strings.Contains(string(raw), "api_server: http://127.0.0.1:") {
		t.Errorf("%q %v", raw, err)
	}
	fi, _ := os.Stat(b.tmpDir)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("tmp dir mode %v", fi.Mode())
	}
}

func TestInvalidRetentionFailsEarly(t *testing.T) {
	opts := fakeOptions(t)
	opts.Retention = "1"
	if _, err := Start(ctx(t), opts); err == nil || !strings.Contains(err.Error(), "unit suffix") {
		t.Errorf("%v", err)
	}
}

func TestRestartAfterCloseStartsNothing(t *testing.T) {
	b, err := Start(ctx(t), fakeOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Restart(ctx(t)); err == nil {
		t.Fatal("Restart after Close succeeded")
	}
	for _, h := range b.Health() {
		if h.Running {
			t.Errorf("%s running after Close and Restart", h.Name)
		}
	}
}
