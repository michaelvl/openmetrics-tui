package dashapp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelvl/openmetrics-tui/internal/managed/fakebin"
)

const dash = `title: T
variables:
  ns: {default: prod}
panels:
  - title: Good
    query: up{namespace="${ns}"}
  - title: Empty
    query: nothing
  - title: Broken
    query: broken
`

func writeDash(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "d.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeBackend(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		if seen != nil {
			*seen = append(*seen, q)
		}
		switch {
		case strings.HasPrefix(q, "broken"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"boom"}`))
		case strings.HasPrefix(q, "nothing"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[` +
				`{"metric":{"a":"1"},"values":[[1700000000,"1"]]},{"metric":{"a":"2"},"values":[[1700000000,"2"]]}]}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckAgainstFakeBackend(t *testing.T) {
	var seen []string
	srv := fakeBackend(t, &seen)
	path := writeDash(t, dash)

	code, out, errOut := run("--check", "--prom", srv.URL, "--var", "ns=staging", path)
	if code != 1 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{
		"ok    Good: 2 rows",
		"ok    Empty: 0 rows (no data)",
		"FAIL  Broken: bad_data: boom",
		"1 of 3 panels failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if len(seen) != 3 || seen[0] != `up{namespace="staging"}` {
		t.Errorf("queries: %q", seen)
	}
}

func TestCheckSuccessUsesDatasourceURL(t *testing.T) {
	srv := fakeBackend(t, nil)
	body := strings.Replace(dash, "title: T\n", "title: T\ndatasource: {url: "+srv.URL+"}\n", 1)
	body = body[:strings.Index(body, "  - title: Empty")]
	code, out, errOut := run("--check", writeDash(t, body))
	if code != 0 {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
}

func TestCheckValidationErrorNeedsNoBackend(t *testing.T) {
	path := writeDash(t, "title: T\npanels:\n  - {title: A, query: 'sum('}\n")
	code, _, errOut := run("--check", "--prom", "http://127.0.0.1:1", path)
	if code != 1 || !strings.Contains(errOut, "d.yaml:3: panel \"A\": invalid query") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestNoBackend(t *testing.T) {
	code, _, errOut := run("--check", writeDash(t, "title: T\npanels:\n  - {title: A, query: up}\n"))
	if code != 2 || !strings.Contains(errOut, "no backend") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestBackendSelectionErrors(t *testing.T) {
	path := writeDash(t, dash)
	code, _, errOut := run("--prom", "http://x", "--managed", "--check", path)
	if code != 2 || !strings.Contains(errOut, "mutually exclusive") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
	for _, args := range [][]string{
		{"--kube-context", "x"}, {"--namespace", "x"}, {"--kubectl-bin", "x"},
		{"--vm-bin", "x"}, {"--data-dir", "x"}, {"--retention", "2d"},
		{"--scrape-config", "x"}, {"--log-file", "x"},
	} {
		code, _, errOut := run(append(args, "--check", path)...)
		if code != 2 || !strings.Contains(errOut, "only apply with --managed") || !strings.Contains(errOut, args[0]) {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
	code, _, errOut = run("--check", writeDash(t, "title: T\npanels:\n  - {title: A, query: up}\n"))
	if code != 2 || !strings.Contains(errOut, "--prom") || !strings.Contains(errOut, "--managed") || !strings.Contains(errOut, "datasource.url") {
		t.Errorf("no-backend message should list all options: %q", errOut)
	}
}

// TestHelperProcess is not a test: the test binary re-executes itself as a
// fake kubectl or victoria-metrics (see fakebin).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Exit(fakebin.Run(args[0], args[1:]))
}

func fakeTool(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nexec '" + os.Args[0] + "' -test.run='^TestHelperProcess$' -- " + name + " \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManagedCheckEndToEnd(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	kubectl, vm := fakeTool(t, "kubectl"), fakeTool(t, "victoria-metrics")
	path := writeDash(t, "title: T\npanels:\n  - {title: Good, query: up}\n")
	logFile := filepath.Join(t.TempDir(), "log")

	code, out, errOut := run("--managed", "--check", "--kubectl-bin", kubectl, "--vm-bin", vm, "--log-file", logFile, path)
	if code != 0 || !strings.Contains(out, "ok    Good: 1 rows") {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "omtui-dash-*")); len(left) != 0 {
		t.Errorf("temp dir left behind: %v", left)
	}
	if raw, _ := os.ReadFile(logFile); !strings.Contains(string(raw), "Starting to serve on 127.0.0.1:") {
		t.Errorf("log file: %q", raw)
	}
}

func TestManagedStartupFailureExitsWithStep(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path := writeDash(t, "title: T\npanels:\n  - {title: Good, query: up}\n")
	code, out, errOut := run("--managed", "--check", "--kubectl-bin", "/nonexistent/kubectl", path)
	if code != 1 || out != "" || !strings.Contains(errOut, "kubectl version --client") || !strings.Contains(errOut, "install kubectl") {
		t.Errorf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	code, _, errOut = run("--managed", "--check", "--retention", "1h", "--kubectl-bin", "/nonexistent/kubectl", path)
	if code != 2 || !strings.Contains(errOut, "24h") {
		t.Errorf("retention: code %d stderr %s", code, errOut)
	}
}

// Without --check the TUI needs a backend, and says so before taking the
// terminal; the placeholder message of earlier phases is gone.
func TestTUIWithoutBackendIsUsageError(t *testing.T) {
	code, _, errOut := run(writeDash(t, dash))
	if code != 2 || !strings.Contains(errOut, "--prom") || strings.Contains(errOut, "not implemented") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestBadVarAndUsage(t *testing.T) {
	path := writeDash(t, dash)
	if code, _, _ := run("--var", "nope=1", "--check", path); code != 2 {
		t.Errorf("unknown var: code %d", code)
	}
	if code, _, _ := run("--var", "novalue", path); code != 2 {
		t.Errorf("malformed --var: code %d", code)
	}
	if code, _, _ := run(); code != 2 {
		t.Errorf("no args: code %d", code)
	}
}

func TestSSHFlagValidation(t *testing.T) {
	path := writeDash(t, dash)
	keys := filepath.Join(t.TempDir(), "authorized_keys")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--ssh-listen", ":0", "--ssh-authorized-keys", keys, "--managed"}, "--managed"},
		{[]string{"--ssh-listen", ":0", "--ssh-authorized-keys", keys, "--check", "--prom", "http://x"}, "--check"},
		{[]string{"--ssh-listen", ":0", "--ssh-authorized-keys", keys, "--http-port", "9999", "--prom", "http://x"}, "--http-port"},
		{[]string{"--ssh-listen", ":0", "--prom", "http://x"}, "--ssh-authorized-keys"},
		{[]string{"--ssh-listen", ":0", "--ssh-allow-any-key", "--ssh-max-sessions", "0", "--prom", "http://x"}, "--ssh-max-sessions"},
		{[]string{"--ssh-allow-any-key", "--prom", "http://x"}, "only apply with --ssh-listen"},
		{[]string{"--ssh-max-sessions", "5", "--prom", "http://x"}, "--ssh-max-sessions"},
	} {
		code, _, errOut := run(append(tc.args, path)...)
		if code != 2 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: code %d, stderr %q", tc.args, code, errOut)
		}
	}
}

func TestPrintScrapeConfig(t *testing.T) {
	path := writeDash(t, "title: T\nscrape:\n  namespaces: [prod]\n  selector: app=foo\npanels:\n  - title: P\n    query: up\n")
	code, out, errOut := run("--print-scrape-config", path)
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"names: [prod]", "label: app=foo", "__meta_kubernetes_pod_ip"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "api_server") {
		t.Errorf("in-cluster config has api_server:\n%s", out)
	}
	// --namespace overrides the dashboard, and the path is optional.
	_, out, _ = run("--print-scrape-config", "--namespace", "other", path)
	if !strings.Contains(out, "names: [other]") {
		t.Errorf("--namespace not applied:\n%s", out)
	}
	if code, out, _ = run("--print-scrape-config"); code != 0 || !strings.Contains(out, "role: pod") {
		t.Errorf("without a dashboard: code %d\n%s", code, out)
	}
}

func TestPrintScrapeConfigExclusive(t *testing.T) {
	path := writeDash(t, dash)
	for _, extra := range [][]string{{"--managed"}, {"--prom", "http://x"}, {"--check"}, {"--var", "ns=a"}, {"--ssh-listen", ":0"}, {"--data-dir", "x"}} {
		code, out, errOut := run(append([]string{"--print-scrape-config"}, append(extra, path)...)...)
		if code != 2 || out != "" || !strings.Contains(errOut, extra[0]) {
			t.Errorf("%v: code %d, stdout %q, stderr %q", extra, code, out, errOut)
		}
	}
}
