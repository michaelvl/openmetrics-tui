# TODO: serve `omtui-dash` over SSH

Checklist for [PLAN-ssh.md](PLAN-ssh.md). Each phase lists its tasks, its tests
and its exit criteria; tick a phase only when the exit criteria hold.

## Phase 0: groundwork and de-risking

- [x] `go get github.com/charmbracelet/wish` (v1 line); `go mod tidy`; `go build ./...` with Bubble Tea 1.3.10 and lipgloss 1.1.0 unchanged
- [x] Stop and decide separately if Wish forces a Bubble Tea or lipgloss upgrade
- [x] Spike in the scratchpad (not committed): Wish server serving `dashtui.New` with a fake `Querier`
- [x] Spike: connect with `ssh -p 2222 -o StrictHostKeyChecking=no localhost`; confirm it draws, resizes and quits
- [x] Spike: note how colours look with the global default renderer (baseline for Phase 3)
- [x] Audit default-renderer colour use and finalise the list: package-level styles in `internal/dashtui/view.go`, `colorStyle`, inline `lipgloss.NewStyle()` in `header.go` and `screen.go`, `Model.input` (`bubbles/textinput`)
- [x] Check what the `textinput` default styles actually render
- [x] Confirm `internal/panel` and `internal/textutil` use only `lipgloss.Width`
- [x] Exit: spike works, audit list is final, no dependency conflict

## Phase 1: split session construction from program start

- [x] `internal/dashtui/run.go`: extract `NewSession(ctx, opts) *Model`; `Run` keeps the local program and the headless HTTP server
- [x] Add `Options.NoSnapshot`; `saveSnapshot` returns early with a clear message when set
- [x] Hide the `S` hint in `footer.go` (the hint is in the help overlay in `screen.go`, footer has none) when `NoSnapshot` is set
- [x] Read `model.go` and `keys.go` for any other use of `os.Stdout`, `os.Stderr` or the working directory
- [x] Test: with `NoSnapshot` the key does nothing, no file is written, the hint is absent
- [x] Existing `dashtui` tests unchanged and green
- [x] Exit: `go test ./...` passes, local `omtui-dash` behaves exactly as before

## Phase 2: SSH server, colours still global

- [x] New package `internal/sshserve` with `Config` and `Serve(ctx, cfg, newModel)`
- [x] Middleware stack: `bubbletea.Middleware`, `activeterm.Middleware` (reject non-PTY with a message), `logging.Middleware`
- [x] Public-key handler that re-reads `AuthKeysPath` on every attempt; accept all with `AllowAnyKey`
- [x] Session cap: counting semaphore middleware, one-line rejection when full
- [x] Graceful shutdown on `ctx.Done()` (`srv.Shutdown`, 5 s timeout)
- [x] Host key: `wish.WithHostKeyPath`; generate ed25519 with mode 0600 when absent; log the fingerprint
- [x] `dashapp.go`: flags `--ssh-listen`, `--ssh-host-key`, `--ssh-authorized-keys`, `--ssh-allow-any-key`, `--ssh-max-sessions` (default 20)
- [x] `dashapp.go`: reject `--ssh-listen` with `--managed`, `--check` or `--http-port` (exit 2)
- [x] `dashapp.go`: require an authorized-keys file or `--ssh-allow-any-key`; warn at startup for the latter
- [x] `dashapp.go`: after `newBackend`, call `sshserve.Serve` with a handler building `dashtui.NewSession` per connection (same `Dashboard`, `Values`, `Querier`; `NoSnapshot: true`) and `tea.WithAltScreen()`
- [x] Startup message on stderr: listen address and host key fingerprint
- [x] Test: accepts a listed key, rejects an unlisted key
- [x] Test: rejects a session without a PTY
- [x] Test: window-size request reaches the model as `tea.WindowSizeMsg`
- [x] Test: session cap rejects the N+1th connection and frees a slot on close
- [x] Test: cancelling the context closes the listener and ends sessions
- [x] Test: flag validation for each incompatible combination
- [x] Exit: `ssh -p 2222 localhost` against `mock-server` shows the real dashboard; two terminals hold independent variable values; quitting one leaves the other running

## Phase 3: per-session colours

### 3a. Theme

- [x] `internal/dashtui/theme.go`: `theme` struct holding a `*lipgloss.Renderer` and the styles `faint`, `err`, `warn`, `ok`, `accent`, `title`, `dashTitle`
- [x] `newTheme(r)` builds every style with `r.NewStyle()`
- [x] Methods `color(name)` (was `colorStyle`) and `box()`
- [x] `Options.Renderer`; `New` falls back to `lipgloss.DefaultRenderer()` and sets `m.th`

### 3b. Call sites

- [x] Delete the package-level style vars in `view.go`; let the compiler list the call sites
- [x] `footer.go`: switch to `m.th.*`
- [x] `screen.go`: switch to `m.th.*`; help overlay box and `lipgloss.Place` whitespace styles from `m.th.r`
- [x] `view.go`: switch to `m.th.*`
- [x] `header.go`: variable boxes, suggestion box, `boxTop` and helpers take the theme or become `Model` methods
- [x] `Model.input` (`textinput`): styles from `m.th.r` if the audit showed they render anything
- [x] Threshold colour names still go through `th.color`
- [x] Snapshot and screenshot output (ANSI stripped) is byte-identical

### 3c. SSH wiring

- [x] In the Phase 2 handler: `r := bubbletea.MakeRenderer(sess)`; pass as `Options.Renderer`
- [x] Do not set `tea.WithInput` or `tea.WithOutput` again (Wish sets them)

### 3d. Tests

- [x] Unit: render a model view for each `termenv` profile (`Ascii`, `ANSI`, `ANSI256`, `TrueColor`) using `lipgloss.NewRenderer(io.Discard)`; `Ascii` has no colour sequences, `ANSI256` contains `38;5;`
- [x] Unit: two models with different renderers in one process do not affect each other
- [x] Existing view goldens pass unchanged
- [x] SSH integration: two PTY requests, `TERM=xterm-256color` and `TERM=dumb`; first frame coloured, second plain
- [x] Exit: two simultaneous clients with different `TERM` both look right; server with stdout on `/dev/null` still colours a 256-colour client; no use of the removed globals remains

## Phase 4: container and sidecar deployment

### 4a. In-cluster scrape config

- [x] `internal/managed/scrape.go`: `ScrapeParams.InCluster`; no `api_server`; `__address__` is `<pod_ip>:<port>`; plain `__metrics_path__` instead of the pod-proxy rewrite
- [x] Keep Running filter, namespace and selector handling, annotation mode, and the `namespace`, `pod`, `instance` labels identical to the proxy variant
- [x] `--print-scrape-config` flag: prints the in-cluster config and exits 0; needs no backend or cluster; usable only with the dashboard path and `--namespace`
- [x] Golden tests for default, selector, fixed port, annotation mode, namespaces
- [x] Test that the two variants differ only in addressing and path labels
- [x] Verify against real VictoriaMetrics using the existing fake Kubernetes API as `api_server`

### 4b. Image and manifests

The image was built with docker and run non-root with a read-only root filesystem and the example's key mounts (SSH served, host key logged). The manifests parse as YAML but were not applied or run through kustomize: there is no kubectl here, so that is left to 4c. The `omtui-dash` memory and CPU in the Deployment were measured at 20 sessions (peak RSS 85 MB); the VictoriaMetrics numbers are estimates, so the resources item stays unticked.

- [x] `Dockerfile` for `omtui-dash` only: multi-stage, `CGO_ENABLED=0`, distroless or scratch, non-root
- [x] `examples/ssh/`: Deployment with two containers (pinned `victoriametrics/victoria-metrics` with `-promscrape.config`, `-retentionPeriod=24h`, `-search.latencyOffset=0`, `emptyDir`; and `omtui-dash --prom http://localhost:8428 --ssh-listen :2222 ...`)
- [x] ConfigMaps for the dashboard and the generated scrape config
- [x] Makefile target regenerating the scrape ConfigMap with `--print-scrape-config`
- [x] Secrets for the host key and the authorized keys
- [x] ServiceAccount, Role/RoleBinding (and a ClusterRole variant) with `list` and `watch` on `pods` only
- [x] Service (ClusterIP; LoadBalancer as a commented option)
- [x] Probes: TCP on the SSH port, HTTP `/health` on VictoriaMetrics
- [ ] Resource requests and limits from a short test at the session cap

### 4c. Verify on kind

Verified on a kind cluster (`make kind-up ssh-image`, keys from `make ssh-example-keys`, `kubectl apply -k examples/ssh`, `kubectl -n omtui port-forward svc/omtui-dash 2222:2222`). Pod restart check: the host key fingerprint was unchanged and a client with `StrictHostKeyChecking=yes` connected; panels were empty again after the restart (history lost with the `emptyDir`).

- [x] Apply the example on the `docs/kubernetes-local.md` kind setup with the demo workloads
- [x] Pods are discovered and scraped; panels fill
- [x] Two SSH sessions work independently
- [x] Pod restart keeps the host key and loses history (documented)
- [x] Exit: `kubectl apply -k examples/ssh` then `ssh -p 2222 <svc>` shows live data on a cluster with no Prometheus

## Phase 5: dashboard catalog

### 5a. Catalog

- [x] `internal/dashboard/catalog.go`: `LoadCatalog(dir)`, `Names()`, `Get(name)`; reads `*.yaml` and `*.yml` (not recursive) with `Load`, sorted by name
- [x] Names match `^[a-z0-9][a-z0-9._-]*$` after stripping the extension; others skipped with a warning
- [x] Duplicate name (`a.yaml` and `a.yml`) is an error; empty directory is an error
- [x] Lookup is a map access only; no user input is joined into a path
- [x] `Reload()`: a file that fails to parse is skipped and reported, the rest stay usable; startup treats any failure as fatal
- [x] Tests: listing and order, invalid file (fatal at startup, skipped on reload), name rules, duplicate name, `../x` and absolute paths not found, empty directory

### 5b. Wiring

- [x] `dashapp.go`: the path argument may be a file or a directory; a file is a catalog of one and behaves as before
- [x] Directory mode requires `--prom` and rejects `--var`
- [x] Warn at startup when a catalog dashboard sets `datasource.url`; ignore it
- [x] `--check` over a directory checks every dashboard
- [x] `--print-scrape-config` with a directory: exactly one dashboard may carry `scrape:`; more than one is an error, none uses the defaults
- [x] SSH handler reloads the catalog per connection and builds the session from the chosen dashboard with its own default `Values`
- [x] Tests: directory with `--var` rejected, without `--prom` rejected, two `scrape:` sections rejected

### 5c. Selection

- [x] Name from `sess.Command()`: exactly one argument, otherwise an error message
- [x] No name and one dashboard: open directly
- [x] No name and several dashboards: picker model (title and name, arrows, Enter, `q`) using the session renderer
- [x] Unknown name: print the available names to the session and exit non-zero
- [x] SSH integration tests: `NAME` opens that dashboard; unknown name lists names; picker then Enter opens one; a file added to the directory appears on the next connection

### 5d. Example

- [x] `examples/ssh/`: mount a directory ConfigMap with two dashboards instead of a single file
- [x] Exit: `ssh -t host team-a` and `ssh -t host team-b` open their own dashboards; plain `ssh host` shows the picker; editing the ConfigMap changes what new connections see

Verified: `go build`, `go vet` and `go test ./...` are green. On the kind cluster `omtui` (image rebuilt, `kubectl apply -k examples/ssh`, deployment restarted, port-forward and a real OpenSSH client in tmux): `ssh -t host team-a` and `team-b` open their own dashboards, plain `ssh host` shows the picker (Enter and `j`+Enter open the first and second), an unknown name prints `Unknown dashboard "nope". Available: team-a, team-b` and exits 1, and replacing the ConfigMap (edited team-b, added team-c) showed up in the picker of a new connection about 30 s later, with no restart. Not verified: panels showed "no data (not scraped?)" on the cluster, so live data in the catalog dashboards was not checked (scraping is Phase 4c, unchanged here). Without `-t` the session is refused by the PTY check, so the unknown-name message needs `-t` too. The example ConfigMap has no name suffix hash so edits apply without a pod restart.

## Phase 6: documentation

- [x] `README.md`: "Serve over SSH" section with flags, auth model and security notes
- [x] `docs/kubernetes-local.md`: sidecar deployment as another way to run it; `--print-scrape-config`
- [x] README: the catalog directory, `ssh -t host NAME`, the picker, and the catalog rules (needs `--prom`, no `--var`, one `scrape:` section)
- [x] State the limits: no snapshot key, no `R` or `L` with the sidecar, history lost on restart without a PVC, no headless HTTP over SSH, colour depth depends on the client
- [x] Note that `SendEnv COLORTERM` is needed for truecolor

## Phase 7 (later): single image with managed mode over SSH

### 7a. In-cluster managed mode

- [x] `--managed --in-cluster` (explicit flag only; `KUBERNETES_SERVICE_HOST` is not auto-detected, so plain `--managed` inside a pod keeps its old meaning)
- [x] Skip `kubectl` startup and preflight; start only VictoriaMetrics with the in-cluster scrape config
- [x] `Health`, `Logs`, `Restart`, `Events` work for the single child; `Restart` no longer restarts a proxy
- [x] Reject `--kubectl-bin` and `--kube-context` with `--in-cluster`
- [x] Existing proxy managed mode unchanged

### 7b. Share the backend between sessions

- [x] Event broadcaster: fan out `Backend.Events()` to every subscriber with non-blocking sends and a small per-subscriber buffer
- [x] `dashtui`: subscribe per model, unsubscribe on session end
- [x] Serialise `Restart`; a second call while one runs returns the same result
- [x] Relax the Phase 2 validation: allow `--ssh-listen` with `--managed --in-cluster`; keep plain `--managed` rejected
- [x] Tests: two subscribers both receive; an unsubscribed one does not block the broadcaster; concurrent `Restart`; flag validation

### 7c. Image

- [x] Second Dockerfile target or build arg with a pinned `victoria-metrics` next to `omtui-dash`, no `kubectl`
- [x] Non-root with a writable `--data-dir`

### 7d. Manifests and verification

- [x] `examples/ssh-single/`: one container, `--managed --in-cluster`, PVC variant
- [x] Run on kind; kill the VictoriaMetrics child; the banner shows in two sessions at once and `R` recovers both
- [x] SSH integration test with a fake managed backend
- [x] Exit: one image, one container, `ssh` shows data, a deliberate child failure is visible and recoverable from any session

Verification notes:

- Unit and integration tests (`go test ./... -race`) cover the broadcaster,
  concurrent `Restart` (one start, shared result and error), in-cluster start
  with no kubectl, flag validation, and an SSH test with a fake managed backend
  (two sessions see the failure, `R` restarts once, both stay subscribed).
- On kind (namespace `omtui-single`, image target `single`): data showed over
  ssh; after `kill -9` of the victoria-metrics child the banner appeared in two
  tmux sessions at once, and `R` in one recovered both (the other on its next
  tick, up to the refresh interval). The PVC variant came up with the claim
  bound and VictoriaMetrics data on it. Not verified: history surviving a pod
  restart on the PVC, and `L` in a live session (only in unit tests).
- Deviations: no `KUBERNETES_SERVICE_HOST` detection; the PVC variant is the
  sibling `examples/ssh-single-pvc/` (a kustomize overlay cannot live inside its
  own base); the dashboards in `examples/ssh-single/` are copies.
