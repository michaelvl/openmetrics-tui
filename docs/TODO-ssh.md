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

- [ ] `internal/dashtui/theme.go`: `theme` struct holding a `*lipgloss.Renderer` and the styles `faint`, `err`, `warn`, `ok`, `accent`, `title`, `dashTitle`
- [ ] `newTheme(r)` builds every style with `r.NewStyle()`
- [ ] Methods `color(name)` (was `colorStyle`) and `box()`
- [ ] `Options.Renderer`; `New` falls back to `lipgloss.DefaultRenderer()` and sets `m.th`

### 3b. Call sites

- [ ] Delete the package-level style vars in `view.go`; let the compiler list the call sites
- [ ] `footer.go`: switch to `m.th.*`
- [ ] `screen.go`: switch to `m.th.*`; help overlay box and `lipgloss.Place` whitespace styles from `m.th.r`
- [ ] `view.go`: switch to `m.th.*`
- [ ] `header.go`: variable boxes, suggestion box, `boxTop` and helpers take the theme or become `Model` methods
- [ ] `Model.input` (`textinput`): styles from `m.th.r` if the audit showed they render anything
- [ ] Threshold colour names still go through `th.color`
- [ ] Snapshot and screenshot output (ANSI stripped) is byte-identical

### 3c. SSH wiring

- [ ] In the Phase 2 handler: `r := bubbletea.MakeRenderer(sess)`; pass as `Options.Renderer`
- [ ] Do not set `tea.WithInput` or `tea.WithOutput` again (Wish sets them)

### 3d. Tests

- [ ] Unit: render a model view for each `termenv` profile (`Ascii`, `ANSI`, `ANSI256`, `TrueColor`) using `lipgloss.NewRenderer(io.Discard)`; `Ascii` has no colour sequences, `ANSI256` contains `38;5;`
- [ ] Unit: two models with different renderers in one process do not affect each other
- [ ] Existing view goldens pass unchanged
- [ ] SSH integration: two PTY requests, `TERM=xterm-256color` and `TERM=dumb`; first frame coloured, second plain
- [ ] Exit: two simultaneous clients with different `TERM` both look right; server with stdout on `/dev/null` still colours a 256-colour client; no use of the removed globals remains

## Phase 4: container and sidecar deployment

### 4a. In-cluster scrape config

- [ ] `internal/managed/scrape.go`: `ScrapeParams.InCluster`; no `api_server`; `__address__` is `<pod_ip>:<port>`; plain `__metrics_path__` instead of the pod-proxy rewrite
- [ ] Keep Running filter, namespace and selector handling, annotation mode, and the `namespace`, `pod`, `instance` labels identical to the proxy variant
- [ ] `--print-scrape-config` flag: prints the in-cluster config and exits 0; needs no backend or cluster; usable only with the dashboard path and `--namespace`
- [ ] Golden tests for default, selector, fixed port, annotation mode, namespaces
- [ ] Test that the two variants differ only in addressing and path labels
- [ ] Verify against real VictoriaMetrics using the existing fake Kubernetes API as `api_server`

### 4b. Image and manifests

- [ ] `Dockerfile` for `omtui-dash` only: multi-stage, `CGO_ENABLED=0`, distroless or scratch, non-root
- [ ] `examples/ssh/`: Deployment with two containers (pinned `victoriametrics/victoria-metrics` with `-promscrape.config`, `-retentionPeriod=24h`, `-search.latencyOffset=0`, `emptyDir`; and `omtui-dash --prom http://localhost:8428 --ssh-listen :2222 ...`)
- [ ] ConfigMaps for the dashboard and the generated scrape config
- [ ] Makefile target regenerating the scrape ConfigMap with `--print-scrape-config`
- [ ] Secrets for the host key and the authorized keys
- [ ] ServiceAccount, Role/RoleBinding (and a ClusterRole variant) with `list` and `watch` on `pods` only
- [ ] Service (ClusterIP; LoadBalancer as a commented option)
- [ ] Probes: TCP on the SSH port, HTTP `/health` on VictoriaMetrics
- [ ] Resource requests and limits from a short test at the session cap

### 4c. Verify on kind

- [ ] Apply the example on the `docs/kubernetes-local.md` kind setup with the demo workloads
- [ ] Pods are discovered and scraped; panels fill
- [ ] Two SSH sessions work independently
- [ ] Pod restart keeps the host key and loses history (documented)
- [ ] Exit: `kubectl apply -k examples/ssh` then `ssh -p 2222 <svc>` shows live data on a cluster with no Prometheus

## Phase 5: dashboard catalog

### 5a. Catalog

- [ ] `internal/dashboard/catalog.go`: `LoadCatalog(dir)`, `Names()`, `Get(name)`; reads `*.yaml` and `*.yml` (not recursive) with `Load`, sorted by name
- [ ] Names match `^[a-z0-9][a-z0-9._-]*$` after stripping the extension; others skipped with a warning
- [ ] Duplicate name (`a.yaml` and `a.yml`) is an error; empty directory is an error
- [ ] Lookup is a map access only; no user input is joined into a path
- [ ] `Reload()`: a file that fails to parse is skipped and reported, the rest stay usable; startup treats any failure as fatal
- [ ] Tests: listing and order, invalid file (fatal at startup, skipped on reload), name rules, duplicate name, `../x` and absolute paths not found, empty directory

### 5b. Wiring

- [ ] `dashapp.go`: the path argument may be a file or a directory; a file is a catalog of one and behaves as before
- [ ] Directory mode requires `--prom` and rejects `--var`
- [ ] Warn at startup when a catalog dashboard sets `datasource.url`; ignore it
- [ ] `--check` over a directory checks every dashboard
- [ ] `--print-scrape-config` with a directory: exactly one dashboard may carry `scrape:`; more than one is an error, none uses the defaults
- [ ] SSH handler reloads the catalog per connection and builds the session from the chosen dashboard with its own default `Values`
- [ ] Tests: directory with `--var` rejected, without `--prom` rejected, two `scrape:` sections rejected

### 5c. Selection

- [ ] Name from `sess.Command()`: exactly one argument, otherwise an error message
- [ ] No name and one dashboard: open directly
- [ ] No name and several dashboards: picker model (title and name, arrows, Enter, `q`) using the session renderer
- [ ] Unknown name: print the available names to the session and exit non-zero
- [ ] SSH integration tests: `NAME` opens that dashboard; unknown name lists names; picker then Enter opens one; a file added to the directory appears on the next connection

### 5d. Example

- [ ] `examples/ssh/`: mount a directory ConfigMap with two dashboards instead of a single file
- [ ] Exit: `ssh -t host team-a` and `ssh -t host team-b` open their own dashboards; plain `ssh host` shows the picker; editing the ConfigMap changes what new connections see

## Phase 6: documentation

- [ ] `README.md`: "Serve over SSH" section with flags, auth model and security notes
- [ ] `docs/kubernetes-local.md`: sidecar deployment as another way to run it; `--print-scrape-config`
- [ ] README: the catalog directory, `ssh -t host NAME`, the picker, and the catalog rules (needs `--prom`, no `--var`, one `scrape:` section)
- [ ] State the limits: no snapshot key, no `R` or `L` with the sidecar, history lost on restart without a PVC, no headless HTTP over SSH, colour depth depends on the client
- [ ] Note that `SendEnv COLORTERM` is needed for truecolor

## Phase 7 (later): single image with managed mode over SSH

### 7a. In-cluster managed mode

- [ ] `--managed --in-cluster` (or detect `KUBERNETES_SERVICE_HOST`)
- [ ] Skip `kubectl` startup and preflight; start only VictoriaMetrics with the in-cluster scrape config
- [ ] `Health`, `Logs`, `Restart`, `Events` work for the single child; `Restart` no longer restarts a proxy
- [ ] Reject `--kubectl-bin` and `--kube-context` with `--in-cluster`
- [ ] Existing proxy managed mode unchanged

### 7b. Share the backend between sessions

- [ ] Event broadcaster: fan out `Backend.Events()` to every subscriber with non-blocking sends and a small per-subscriber buffer
- [ ] `dashtui`: subscribe per model, unsubscribe on session end
- [ ] Serialise `Restart`; a second call while one runs returns the same result
- [ ] Relax the Phase 2 validation: allow `--ssh-listen` with `--managed --in-cluster`; keep plain `--managed` rejected
- [ ] Tests: two subscribers both receive; an unsubscribed one does not block the broadcaster; concurrent `Restart`; flag validation

### 7c. Image

- [ ] Second Dockerfile target or build arg with a pinned `victoria-metrics` next to `omtui-dash`, no `kubectl`
- [ ] Non-root with a writable `--data-dir`

### 7d. Manifests and verification

- [ ] `examples/ssh-single/`: one container, `--managed --in-cluster`, PVC variant
- [ ] Run on kind; kill the VictoriaMetrics child; the banner shows in two sessions at once and `R` recovers both
- [ ] SSH integration test with a fake managed backend
- [ ] Exit: one image, one container, `ssh` shows data, a deliberate child failure is visible and recoverable from any session
