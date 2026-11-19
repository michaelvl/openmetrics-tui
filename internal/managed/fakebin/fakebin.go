// Package fakebin implements fake kubectl and victoria-metrics processes for
// tests. A test binary re-executes itself as one of them (the TestHelperProcess
// pattern) and calls Run. Behaviour is chosen with the FAKE_MODE environment
// variable (both tools), FAKE_MODE_KUBECTL or FAKE_MODE_VM, each a comma
// separated list of:
//
//	hang         never print the ready line / never serve readiness
//	crash        run normally, then exit 1 shortly after becoming ready
//	crash-start  print an error and exit 1 immediately
//	ignore-term  ignore SIGTERM (forces the SIGKILL escalation)
//	norbac       kubectl auth can-i answers "no"
//	clash-once   victoria-metrics fails with "address already in use" once
//	             (state kept in FAKE_STATE_FILE)
//	kubeapi      the proxy also serves a fake pod API and pod-proxy metrics
//
// FAKE_ARGS_FILE, when set, receives the command line of every start.
package fakebin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// shortName selects the per-tool variable: FAKE_MODE_KUBECTL or FAKE_MODE_VM.
var shortName string

func has(mode string) bool {
	for _, v := range []string{"FAKE_MODE", "FAKE_MODE_" + shortName} {
		for _, m := range strings.Split(os.Getenv(v), ",") {
			if m == mode {
				return true
			}
		}
	}
	return false
}

// Run acts as tool ("kubectl" or "victoria-metrics") and returns an exit code.
func Run(tool string, args []string) int {
	shortName = map[string]string{"kubectl": "KUBECTL", "victoria-metrics": "VM"}[tool]
	if f := os.Getenv("FAKE_ARGS_FILE"); f != "" {
		if fh, err := os.OpenFile(f, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			_, _ = fmt.Fprintf(fh, "%s %s\n", tool, strings.Join(args, " "))
			_ = fh.Close()
		}
	}
	if has("ignore-term") {
		signal.Ignore(syscall.SIGTERM)
	}
	switch tool {
	case "kubectl":
		return kubectl(args)
	case "victoria-metrics":
		return vm(args)
	}
	fmt.Fprintln(os.Stderr, "fake: unknown tool", tool)
	return 2
}

func serve(handler http.Handler, announce func(addr string)) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return serveOn(l, handler, announce)
}

func serveOn(l net.Listener, handler http.Handler, announce func(addr string)) int {
	go func() { _ = http.Serve(l, handler) }()
	if !has("hang") {
		announce(l.Addr().String())
	}
	if has("crash") {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "fake: crashing")
		return 1
	}
	waitTerm()
	return 0
}

// waitTerm blocks until SIGTERM, or forever when SIGTERM is ignored.
func waitTerm() {
	term := make(chan os.Signal, 1)
	if has("ignore-term") {
		signal.Notify(term, syscall.SIGINT) // Notify would re-enable SIGTERM
	} else {
		signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	}
	<-term
}

func kubectl(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println("Client Version: v1.31.0")
		return 0
	case "auth":
		if has("norbac") {
			fmt.Println("no")
			return 1
		}
		fmt.Println("yes")
		return 0
	case "proxy":
		if has("crash-start") {
			fmt.Fprintln(os.Stderr, "fake: failed to start: boom")
			return 1
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "ok") })
		if has("kubeapi") {
			mux.Handle("/api/", kubeAPI())
		}
		return serve(mux, func(addr string) {
			fmt.Printf("Starting to serve on %s\n", addr)
		})
	}
	return 2
}

func vm(args []string) int {
	var listen, cfg string
	for _, a := range args {
		switch {
		case a == "-version":
			fmt.Println("1.156.0")
			return 0
		case strings.HasPrefix(a, "-httpListenAddr="):
			listen = strings.TrimPrefix(a, "-httpListenAddr=")
		case strings.HasPrefix(a, "-promscrape.config="):
			cfg = strings.TrimPrefix(a, "-promscrape.config=")
		}
	}
	if has("crash-start") {
		fmt.Fprintln(os.Stderr, "fake: failed to start: boom")
		return 1
	}
	if has("clash-once") {
		if f := os.Getenv("FAKE_STATE_FILE"); f != "" {
			if _, err := os.Stat(f); err != nil {
				_ = os.WriteFile(f, []byte("x"), 0o600)
				fmt.Fprintf(os.Stderr, "fake: cannot listen on %s: bind: address already in use\n", listen)
				return 1
			}
		}
	}
	if _, err := os.Stat(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fake: missing scrape config:", err)
		return 1
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake:", err)
		return 1
	}
	if has("hang") {
		_ = l.Close() // nothing answers /health
		l = nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "OK") })
	handler := func(w http.ResponseWriter, _ *http.Request) {
		now := float64(time.Now().Unix())
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"p"},"values":[[%f,"1"]]}]}}`, now)
	}
	mux.HandleFunc("/api/v1/query_range", handler)
	mux.HandleFunc("/api/v1/query", handler)
	if l == nil {
		waitTerm()
		return 0
	}
	return serveOn(l, mux, func(string) {
		fmt.Printf("started VictoriaMetrics at %s\n", listen)
	})
}

// KubeAPI returns the fake pod API for tests that talk to it directly, for
// example as the api_server of an in-cluster scrape config.
func KubeAPI() http.Handler { return kubeAPI() }

type obj = map[string]any

func pod(name string, ann, labels obj, phase string, containers ...[]int) obj {
	var cs []obj
	for i, ports := range containers {
		c := obj{"name": fmt.Sprintf("c%d", i), "image": "x"}
		var ps []obj
		for _, p := range ports {
			ps = append(ps, obj{"containerPort": p, "protocol": "TCP"})
		}
		if len(ps) > 0 {
			c["ports"] = ps
		}
		cs = append(cs, c)
	}
	md := obj{"name": name, "namespace": "prod", "uid": "uid-" + name, "resourceVersion": "1"}
	if ann != nil {
		md["annotations"] = ann
	}
	if labels != nil {
		md["labels"] = labels
	}
	return obj{"metadata": md, "spec": obj{"containers": cs, "nodeName": "n1"},
		"status": obj{"phase": phase, "podIP": "10.0.0.1", "hostIP": "10.1.1.1"}}
}

// kubeAPI is a minimal Kubernetes API: pod list and watch, and pod-proxy
// metrics for every pod. Pods (namespace prod):
//
//	pod-a  annotated, port 9100, path /custom
//	pod-b  annotated, port 9101, default path
//	pod-c  not annotated, label app=foo, port 8080
//	pod-d  annotated but Pending
//	pod-e  annotated, port 9103, declares 9103 and 9104
//	pod-f  annotated, port 9105, two containers
//	pod-g  not annotated, label app=foo, no declared port
//	pod-h  annotated without a port annotation
func kubeAPI() http.Handler {
	scrape := func(port, path string) obj {
		a := obj{"prometheus.io/scrape": "true", "prometheus.io/port": port}
		if path != "" {
			a["prometheus.io/path"] = path
		}
		return a
	}
	pods := []obj{
		pod("pod-a", scrape("9100", "/custom"), obj{"app": "foo"}, "Running", []int{9100}),
		pod("pod-b", scrape("9101", ""), obj{"app": "foo"}, "Running", []int{9101}),
		pod("pod-c", nil, obj{"app": "foo"}, "Running", []int{8080}),
		pod("pod-d", scrape("9102", ""), nil, "Pending", []int{9102}),
		pod("pod-e", scrape("9103", ""), obj{"app": "bar"}, "Running", []int{9103, 9104}),
		pod("pod-f", scrape("9105", ""), obj{"app": "bar"}, "Running", []int{9105}, []int{9106}),
		pod("pod-g", nil, obj{"app": "foo"}, "Running"),
		pod("pod-h", obj{"prometheus.io/scrape": "true"}, obj{"app": "bar"}, "Running", []int{9107}),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/api/v1/namespaces/prod/pods" || p == "/api/v1/pods":
			if r.URL.Query().Get("watch") != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(time.Minute):
				}
				return
			}
			sel := r.URL.Query().Get("labelSelector")
			var items []obj
			for _, po := range pods {
				if k, v, ok := strings.Cut(sel, "="); ok {
					l, _ := po["metadata"].(obj)["labels"].(obj)
					if l[k] != v {
						continue
					}
				}
				items = append(items, po)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(obj{"kind": "PodList", "apiVersion": "v1",
				"metadata": obj{"resourceVersion": "1"}, "items": items})
		case strings.Contains(p, "/proxy/"):
			_, _ = fmt.Fprintf(w, "probe_hits 1\nprobe_path{path=%q} 1\n", p)
		default:
			http.NotFound(w, r)
		}
	})
}
