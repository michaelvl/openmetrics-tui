# Using omtui-dash with a Kubernetes cluster

`omtui-dash` shows PromQL dashboards. For a cluster there are three ways to give
it a backend:

1. [Managed mode](#managed-mode) (recommended for ad-hoc use): `omtui-dash`
   starts `kubectl proxy` and a local VictoriaMetrics that scrapes your pods
   through the Kubernetes API. Nothing is installed in the cluster.
2. [The manual recipe](#manual-recipe): the same setup, started by hand, for when
   you want to control or reuse the pieces.
3. [An existing backend in the cluster](#an-existing-backend-in-the-cluster),
   reached with `kubectl port-forward` and `--prom`.

The flags and the dashboard YAML are described in the
[README](../README.md#omtui-dash-promql-dashboard).

## Managed mode

```sh
omtui-dash --managed my-dashboard.yaml
```

### Prerequisites

- **`kubectl`**, installed and authenticated by you. `omtui-dash` never touches
  credentials; it runs your `kubectl` and so uses your kubeconfig, including
  exec plugins and SSO logins. Use a `kubectl` within one minor version of the
  cluster (the Kubernetes version skew policy). It is not version-checked.
  Pick the cluster with `--kube-context`, or the current context is used.
- **`victoria-metrics`**. The Nix package (`nix run
  github:michaelvl/openmetrics-tui#omtui-dash`) bundles it and appends it to the
  `PATH` of `omtui-dash`, so one already on your `PATH` wins. With `go install`
  put `victoria-metrics` on your `PATH` or pass `--vm-bin`. Only 1.156.0 and
  1.154.0 were tried; below 1.100.0 a warning is printed.
- **RBAC.** Your user needs, in the namespaces you scrape:
  - `get` on `pods/proxy` (a scrape is a GET through the API server's pod proxy),
  - `list` and `watch` on `pods` (pod discovery).

  Before the dashboard opens, `omtui-dash` runs `kubectl version --client`,
  `kubectl auth can-i get pods --subresource=proxy` and `kubectl auth can-i list
  pods`, so a login prompt or a missing permission shows up in the normal terminal
  with the failing step named. An example `Role`:

  ```yaml
  apiVersion: rbac.authorization.k8s.io/v1
  kind: Role
  metadata:
    name: omtui-dash
    namespace: prod
  rules:
    - apiGroups: [""]
      resources: [pods]
      verbs: [list, watch]
    - apiGroups: [""]
      resources: [pods/proxy]
      verbs: [get]
  ```

  For all namespaces use a `ClusterRole` and `ClusterRoleBinding`. The `can-i`
  preflight only checks `get pods/proxy` and `list pods`; `watch` is not checked.

### What it starts

In order: the preflight above, `kubectl proxy --port=0 --address=127.0.0.1` (the
port it chose is read from its output), a generated VictoriaMetrics scrape config
in a private temporary directory, and `victoria-metrics` listening on a free
local port with `-search.latencyOffset=0` (without it VictoriaMetrics hides the
newest 30 seconds, and the `now` column would lag). All three steps must succeed
before the dashboard opens; otherwise `omtui-dash` exits and prints the failing
step and the tail of its output.

The children are stopped (SIGTERM, then SIGKILL after 5 seconds) when you quit,
and the temporary directory is removed. Their output never goes to the
terminal. Press `L` in the dashboard to read it, or use `--log-file FILE`. If a
child dies, a banner shows its last log lines and `R` restarts it.

### What gets scraped

Scraping is set by the `scrape:` section of the dashboard YAML, not by variables.

```yaml
scrape:
  namespaces: [prod]                  # required when the section is present
  interval: 15s                       # default 15s
  annotation: prometheus.io/scrape    # opt-in by annotation, or:
  # selector: app.kubernetes.io/name=my-controller
  port: prometheus.io/port            # annotation mode: annotation names
  path: prometheus.io/path
```

- With no `scrape:` section, every `Running` pod that declares a container port,
  in all namespaces, is scraped on `/metrics`. Set `--namespace` to narrow it.
- `selector` scrapes pods matching a label selector. Without `scrape.port`, every
  declared container port of a matching pod is scraped, so set `port: 8080` (a
  number) to scrape one. `path` is a literal path, default `/metrics`.
- `annotation` scrapes pods whose annotation of that name is `"true"` and which
  carry the port annotation. The path annotation, if present, gives the path;
  otherwise `/metrics`.
- Pods that declare no container ports are skipped, even with a fixed
  `scrape.port` (VictoriaMetrics creates no target for them).
- Each series gets `namespace`, `pod` and `instance` labels, and the target's own
  labels. There is no `container` label: scraping is per pod and port.
- Metrics served over TLS are not supported by the generated config.
- `--namespace` overrides `scrape.namespaces`. `$namespace` in a dashboard only
  filters queries; if it names a namespace that is not scraped, the panels read
  "no data (not scraped?)".

To use your own configuration, pass `--scrape-config FILE`. `${KUBE_PROXY}` in
the file is replaced with the proxy address (`127.0.0.1:PORT`); see
[the manual recipe](#manual-recipe) for a starting point.

### Cold start and `--data-dir`

By default the storage is a fresh temporary directory, deleted on exit. A fresh
store has no history, so a `1h` column is empty until an hour has been collected.
Until then the dashboard shows `·` in columns that reach before the first sample and
"collecting data (N min)" on the panel title.

To keep history between runs, give a directory:

```sh
omtui-dash --managed --data-dir ~/.cache/omtui-dash/prod my-dashboard.yaml
```

Panels then fill in immediately on the next start. Samples are kept for
`--retention` (default `24h`; a unit suffix is required, and VictoriaMetrics
rejects less than 24h). Gaps while the dashboard was not running stay gaps.
Use one data directory per cluster and scrape setup, and do not run two
instances on the same directory (VictoriaMetrics does not support sharing one).

### Load and limits

Every scrape goes through the Kubernetes API server's pod proxy, which means
one API request per target per scrape interval, in addition to the pod list and
watch. This is fine for looking at a handful of controllers in one namespace; it
is not meant for large clusters.

- Narrow `scrape.namespaces`, prefer `selector` or `annotation` over scraping
  everything, and raise `interval` (30s or 60s) when scraping many pods.
- `pods/proxy` is an access path you may not want to hand out widely; it can
  reach any port of a pod in the namespaces it is granted for.
- Throughput through the proxy and API server rate limits were not measured.
- Managed mode targets Linux and macOS. Child processes are tied to the
  dashboard's lifetime on Linux even when it is killed hard; elsewhere a hard
  kill can leave `kubectl proxy` or `victoria-metrics` running.

## Manual recipe

Run the same pieces yourself. In one terminal, a proxy to the cluster:

```sh
kubectl proxy --port=8001 --address=127.0.0.1
```

Save this as `vm-scrape.yml`. It is what managed mode generates for a dashboard
without a `scrape:` section, with the proxy at `127.0.0.1:8001`. It scrapes every
declared container port of every Running pod on `/metrics`, through the pod proxy:

```yaml
scrape_configs:
  - job_name: pods-via-proxy
    scrape_interval: 15s
    scheme: http
    kubernetes_sd_configs:
      - role: pod
        api_server: http://127.0.0.1:8001
        # namespaces:
        #   names: [prod]
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_phase]
        action: keep
        regex: Running
      - source_labels: [__meta_kubernetes_pod_container_port_number]
        action: keep
        regex: .+
      - source_labels: [__meta_kubernetes_namespace, __meta_kubernetes_pod_name, __meta_kubernetes_pod_container_port_number]
        regex: (.+);(.+);(.+)
        replacement: /api/v1/namespaces/$1/pods/$2:$3/proxy/metrics
        target_label: __metrics_path__
      - replacement: 127.0.0.1:8001
        target_label: __address__
      - source_labels: [__meta_kubernetes_namespace]
        target_label: namespace
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
      - source_labels: [__meta_kubernetes_pod_name, __meta_kubernetes_pod_container_port_number]
        regex: (.+);(.+)
        replacement: $1:$2
        target_label: instance
```

Each target is addressed at the proxy, and the metrics path is rewritten to
`/api/v1/namespaces/NS/pods/POD:PORT/proxy/metrics`. Uncomment `namespaces` to
limit discovery. For annotation or selector based variants see the files in
`internal/managed/testdata/scrape/` (`annotation.golden`, `selector.golden`,
`selector-port-path.golden`), which are the generator's test fixtures.

In a second terminal, VictoriaMetrics:

```sh
victoria-metrics \
  -promscrape.config=vm-scrape.yml \
  -storageDataPath=./vm-data \
  -httpListenAddr=127.0.0.1:8428 \
  -retentionPeriod=24h \
  -search.latencyOffset=0
```

`-retentionPeriod` below 24h is rejected, and a bare number means months, so give
a unit. `-search.latencyOffset=0` keeps the newest samples visible. You can check
the config before starting with `-promscrape.config.dryRun`.

Then point the dashboard at it:

```sh
omtui-dash --prom http://127.0.0.1:8428 my-dashboard.yaml
```

The `scrape:` section is ignored in this mode. The same setup can be handed to
managed mode with your edited file: `omtui-dash --managed --scrape-config
vm-scrape.yml my-dashboard.yaml` (write the proxy address as `${KUBE_PROXY}`
instead of `127.0.0.1:8001`).

## An existing backend in the cluster

If the cluster already runs Prometheus or VictoriaMetrics, skip managed mode and
reach it with a port-forward:

```sh
kubectl -n monitoring port-forward svc/prometheus-server 9090:80
omtui-dash --prom http://localhost:9090 my-dashboard.yaml
```

Replace the namespace, service and ports with your own (VictoriaMetrics usually
listens on 8428, the Prometheus Operator's service on 9090). Any
Prometheus-compatible query API works. If the backend needs credentials, use
`--prom-token-file` or `--prom-user` with `--prom-password-file`. Your queries
then run against whatever the backend has scraped, so `$namespace`-style
variables behave as ordinary filters and panels can reach all of its history.

`omtui-dash --check --prom ... my-dashboard.yaml` is a quick way to confirm that
the dashboard's queries return data before opening the TUI.

## Trying it on a throwaway kind cluster

`make kind-up` creates a kind cluster named `omtui` and deploys demo workloads
into the `demo` namespace (`examples/kind/workloads.yaml`): a web app with
metrics, a load generator, a crash-looping pod and kube-state-metrics. Then
`make kind-dash` runs `examples/kind-dashboard.yaml` in managed mode against it,
and `make kind-down` deletes the cluster. It needs `kind`, a container runtime
and `kubectl`.
