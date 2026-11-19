# Plan: serve `omtui-dash` over SSH

## Goal

Let `omtui-dash` run as a long-lived service, for example a Deployment on
Kubernetes, that users reach with `ssh -p 2222 host`. Each connection gets its
own dashboard session with a real PTY, resize handling and the colour depth of
the client's terminal. No HTTP, no browser terminal, no shell in the container.

A native Prometheus cannot be assumed in the cluster, so the deployment brings
its own VictoriaMetrics. This is delivered in two steps:

1. **Sidecar (Phases 0 to 6).** A pod with two containers: the official
   VictoriaMetrics image scraping pods through in-cluster service discovery, and
   `omtui-dash --prom http://localhost:8428 --ssh-listen :2222`. `omtui-dash`
   stays in external mode, so the SSH server needs no managed-mode support.
2. **Single image (Phase 7, later).** One image with `omtui-dash` and
   `victoria-metrics`, started with `--managed` using in-cluster discovery, so
   the UI keeps its health banner, logs and restart over SSH. No `kubectl`.

Sessions share one backend and one `promapi.Querier` in both cases.

A deployment can serve several dashboards from a directory (a ConfigMap), and
each connection picks one (Phase 5). Dashboards are admin-supplied; users cannot
upload their own in v1 (see Follow-ups).

Non-goals: user management, certificates or OIDC (v1 is public-key auth only),
user-supplied dashboards, a shared query cache across sessions (see Follow-ups),
bundling `kubectl` in the image, changing local behaviour of `omtui-dash`.

## Decisions

| Topic            | Decision                                                                                                                                                                  |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Library          | `github.com/charmbracelet/wish` v1.x with its `bubbletea` middleware. The repo is on Bubble Tea v1.3.10, so stay on the v1 line of Wish; do not pull in the v2 modules.     |
| Entry point      | `omtui-dash --ssh-listen :2222 dashboard.yaml`. Same binary, a new mode instead of a new command. No TUI on the server's own terminal in this mode.                       |
| Auth             | Public key only. `--ssh-authorized-keys FILE` (OpenSSH format, re-read on every connection so a Secret update needs no restart). Without it the server refuses to start. `--ssh-allow-any-key` is an explicit opt-in. |
| Host key         | `--ssh-host-key FILE`. If missing, generate and write it (ed25519) so a single-replica dev setup works; in Kubernetes mount it from a Secret so it survives restarts.      |
| Backend          | Phases 0 to 6: external only. `--ssh-listen` together with `--managed`, `--check` or `--http-port` is a usage error (exit 2). Phase 7 lifts the `--managed` restriction for the in-cluster mode only. |
| Scrape in cluster | VictoriaMetrics uses `kubernetes_sd_configs` without `api_server` (the pod's ServiceAccount) and scrapes pod IPs directly. No `kubectl proxy`, so no API-server proxy load. One generator variant serves both the sidecar and Phase 7. |
| Scrape config source | `omtui-dash --print-scrape-config dashboard.yaml` renders the config from the dashboard's `scrape:` section, so the sidecar's ConfigMap and the dashboard cannot drift apart. |
| Dashboards       | The dashboard argument is a file or a directory. A directory is a catalog of `*.yaml` and `*.yml` files (not recursive); the name is the file name without extension. The catalog is re-read on every connection, so a ConfigMap update needs no restart. Names come only from that listing, never from joining user input into a path. |
| Dashboard choice | `ssh -t host NAME` opens that dashboard. With no name, a single dashboard opens directly and several show a picker. An unknown name prints the available names and ends the session. Switching means reconnecting. |
| Catalog rules    | A directory needs `--prom` (each dashboard's `datasource.url` is ignored with a startup warning) and rejects `--var`, since `Values` refuses variables a dashboard does not declare. At most one dashboard may carry a `scrape:` section; it is the source for `--print-scrape-config`. An invalid file fails startup, and is skipped with a log line if it breaks later. |
| Sessions         | One `dashtui.Model` per connection, sharing the `Querier`. Variables, scroll and filters are per session. Concurrency is capped by `--ssh-max-sessions` (default 20).      |
| Colours          | Per-session `lipgloss.Renderer` from `bubbletea.MakeRenderer(sess)`, held on the model through a `theme` struct. No global colour profile.                                |
| Snapshot key `S` | Disabled over SSH (no meaningful server filesystem). The footer hint is hidden and the key is a no-op.                                                                    |
| Local behaviour  | Unchanged. The local path builds the theme from `lipgloss.DefaultRenderer()`, so goldens and snapshots stay byte-identical.                                              |

## Phase 0: groundwork and de-risking (0.25 day)

Confirm the assumptions the rest relies on before writing code.

1. Add Wish and check it resolves against Bubble Tea v1.3.10 and
   lipgloss v1.1.0 (`go get`, `go mod tidy`, `go build ./...`). If it forces a
   Bubble Tea upgrade, stop and decide separately.
2. Throwaway spike (scratchpad, not committed): a 30-line Wish server serving
   `dashtui.New` with a fake `Querier`. Connect with `ssh -p 2222 -o
   StrictHostKeyChecking=no localhost` and confirm it draws, resizes and quits.
3. In the same spike, record what the colours look like with the global
   default renderer (expected: none or stale, because it samples the server's
   stdout). This is the baseline Phase 3 fixes.
4. Audit colour use: list every place that renders through the default
   renderer. Known so far: the seven package-level styles in
   `internal/dashtui/view.go:43-49`, `colorStyle` (`view.go:54`), the inline
   `lipgloss.NewStyle()` calls in `header.go` and `screen.go`, and the
   `textinput` styles in `Model.input`. `internal/panel` and `internal/textutil`
   use only `lipgloss.Width`, which needs no renderer.

Result (done): Wish v1.4.7 (with `charmbracelet/ssh`) resolves with Bubble Tea
1.3.10, lipgloss 1.1.0, bubbles 0.21.0, x/ansi 0.10.1 and termenv 0.16.0
unchanged; it only raises `creack/pty` to 1.1.21 and adds small x/ deps.
`go mod tidy` drops Wish until something imports it, so Phase 2 re-adds it.
The spike served `dashtui.New` over Wish and a PTY client drew the dashboard,
followed a window resize (120x36 to 80x20) and quit on `q`, leaving the server
up. Colour baseline: the server's stdout was not a terminal, so the default
renderer picked the Ascii profile and the session carried no colour escapes at
all (not even faint or bold). Phase 3 must therefore build styles from the
session renderer.

Final audit of default-renderer use, all in `internal/dashtui`:

- `view.go`: `faintStyle`, `errStyle`, `warnStyle`, `okStyle`, `accentStyle`,
  `titleStyle`, `dashTitleStyle`, and `colorStyle` (`lipgloss.NewStyle()` per
  call).
- `header.go`: variable box border colours and `captionStyle`
  (`lipgloss.NewStyle().Foreground(...)`), the box `Border`/`BorderForeground`
  style, and the edit-box style at `header.go:221`.
- `screen.go`: the help-overlay box style (`screen.go:126`, border, background
  and foreground) and the `lipgloss.Place` whitespace foreground.
- `Model.input` (`textinput.New()`): `PromptStyle` and `TextStyle` are zero
  styles with no attributes. `PlaceholderStyle` and `CompletionStyle` are grey
  240 but unused: no placeholder is set and suggestions are off. The static
  cursor renders `Cursor.Style.Reverse(true)`, a zero style plus reverse, which
  still goes through the default renderer, so it also needs the session
  renderer (set `Cursor.Style`, `PromptStyle`, `TextStyle` from it).
- `internal/panel` and `internal/textutil`: only `lipgloss.Width` (`panel`
  does not import lipgloss at all), so nothing to change.

Exit criteria: spike works, audit list is final, no dependency conflict.

## Phase 1: split session construction from program start (0.5 day)

No behaviour change. This is a pure refactor with the existing tests as the
safety net.

1. `internal/dashtui/run.go`: factor `Run` into
   - `NewSession(ctx, opts) *Model`: the `opts.Ctx = ctx; New(opts)` part, and
   - `Run`, which keeps the local program and the headless HTTP server.
2. Add `Options.NoSnapshot bool`. `saveSnapshot` returns early with a clear
   message when set, and the footer omits the `S` hint (`footer.go`).
3. Make sure nothing in `Model` touches `os.Stdout`, `os.Stderr` or the working
   directory except `saveSnapshot`. A grep and a read of `model.go` and
   `keys.go` should be enough.

Tests: existing `dashtui` suite unchanged and green; one new test for
`NoSnapshot` (key does nothing, no file written, hint absent).

## Phase 2: SSH server, with colours still global (0.75 day)

Get end-to-end working first, so Phase 3 changes can be checked against it.

1. New package `internal/sshserve`:
   - `type Config struct { Listen, HostKeyPath, AuthKeysPath string; AllowAnyKey bool; MaxSessions int }`.
   - `Serve(ctx, cfg, newModel func(ctx context.Context, sess ssh.Session) (tea.Model, []tea.ProgramOption)) error`.
   - Middleware stack: `bubbletea.Middleware(handler)`, `activeterm.Middleware()`
     (reject non-PTY sessions with a message), `logging.Middleware()`.
   - Public-key handler reading `AuthKeysPath` on every attempt. With
     `AllowAnyKey` it accepts everything.
   - Session cap: a counting semaphore in a small middleware, rejecting with a
     one-line message when full.
   - Graceful shutdown: on `ctx.Done()` call `srv.Shutdown` with a 5 s timeout.
2. Host key: `wish.WithHostKeyPath`; generate when absent, mode 0600, and log
   the fingerprint at startup.
3. `internal/dashapp/dashapp.go`:
   - New flags `--ssh-listen`, `--ssh-host-key`, `--ssh-authorized-keys`,
     `--ssh-allow-any-key`, `--ssh-max-sessions`.
   - Validation next to the existing `--prom`/`--managed` checks: SSH is
     incompatible with `--managed`, `--check` and `--http-port`; it needs either
     an authorized-keys file or `--ssh-allow-any-key`.
   - After `newBackend`, if `--ssh-listen` is set, call `sshserve.Serve` with a
     handler that builds `dashtui.NewSession` per connection (same
     `Dashboard`, `Values` and `Querier`; `NoSnapshot: true`) and returns
     `tea.WithAltScreen()`.
   - Per-session `--var` values come from the CLI defaults. SSH command-line
     arguments (`ssh host -- var=x`) are out of scope.
4. Startup message on stderr: listen address and host key fingerprint.

Tests (in `sshserve`, using `golang.org/x/crypto/ssh` as the client against a
`127.0.0.1:0` listener and a stub model):
- accepts a listed key, rejects an unlisted key;
- rejects a session without a PTY;
- window-size request reaches the model as `tea.WindowSizeMsg`;
- the session cap rejects the N+1th connection and frees a slot on close;
- cancelling the context closes the listener and ends sessions.

Exit criteria: `ssh -p 2222 localhost` against `mock-server` shows the real
dashboard, two terminals can hold independent variable values, quitting one
leaves the other running.

## Phase 3: per-session colours (1.5 days)

The core problem: lipgloss v1 styles created with `lipgloss.NewStyle()` render
through the default renderer, which probes the server's stdout. In a container
that is not a terminal, so every client would get the wrong colour profile.
The fix is to render each session through its own renderer.

### 3a. Introduce a theme (0.5 day)

1. New `internal/dashtui/theme.go`:
   ```go
   type theme struct {
       r                                   *lipgloss.Renderer
       faint, err, warn, ok, accent, title, dashTitle lipgloss.Style
   }
   func newTheme(r *lipgloss.Renderer) *theme
   func (t *theme) color(name string) lipgloss.Style   // was colorStyle
   func (t *theme) box() lipgloss.Style                // rounded border + 63
   ```
   `newTheme` builds every style with `r.NewStyle()`. Colour constants
   (`colRed`, and so on) stay where they are.
2. `Options.Renderer *lipgloss.Renderer`. `New` uses it, or
   `lipgloss.DefaultRenderer()` when nil, and stores `m.th = newTheme(r)`.

### 3b. Move every call site onto the theme (0.5 day)

Mechanical, compiler-guided: delete the package-level style vars in
`view.go:43-49` and fix the errors.

- `footer.go`, `screen.go`, `view.go`, `header.go`: `faintStyle` becomes
  `m.th.faint`, and so on. Helpers that are plain functions today (`boxTop`,
  `colorStyle`, the help overlay and variable box builders) take the `*theme` or
  become methods on `Model`.
- Inline `lipgloss.NewStyle()` calls in `header.go` (variable boxes, the
  suggestion box) and `screen.go` (help overlay, `lipgloss.Place` whitespace
  options) become `m.th.r.NewStyle()`. `lipgloss.Place` itself is
  renderer-free; only its whitespace style options use the renderer, so pass
  styles built from `m.th.r`.
- `lipgloss.Width`, `ansi.Truncate`, `ansi.Strip` need no change.
- `Model.input` (`bubbles/textinput`): build it with its styles taken from
  `m.th.r` (`PromptStyle`, `TextStyle`, `PlaceholderStyle`, `Cursor.Style`).
  Check what the current defaults render; if they render nothing, leave them.
- Colour names coming from dashboard thresholds still go through `th.color`.

Snapshot and screenshot paths strip ANSI, so their output must not change.

### 3c. Wire the SSH renderer (0.25 day)

In the Phase 2 handler: `r := bubbletea.MakeRenderer(sess)`; pass it as
`Options.Renderer`. Also use `tea.WithOutput`/`tea.WithInput` as the Wish
middleware already sets them; do not set them again.

Note for docs: the profile comes from the client's `TERM` and, for truecolor,
`COLORTERM`, which OpenSSH only forwards with `SendEnv COLORTERM` (and a server
that accepts it). 256-colour is the practical default and is all the dashboard
uses (`"63"`, `"240"` and so on).

### 3d. Tests for colours (0.25 day)

- Unit: for each `termenv` profile (`Ascii`, `ANSI`, `ANSI256`, `TrueColor`)
  build a renderer with `lipgloss.NewRenderer(io.Discard)`, call
  `r.SetColorProfile(p)`, render a model view and assert: `Ascii` has no `\x1b[`
  colour sequences, `ANSI256` contains `38;5;`. This also proves two models with
  different renderers do not affect each other in one process.
- Golden tests: existing view goldens run with the default renderer pinned to
  `Ascii` or the profile they use today, and must pass unchanged.
- SSH integration: connect twice with PTY requests carrying
  `TERM=xterm-256color` and `TERM=dumb`; assert the first frame of the first has
  colour sequences and the second has none.

Exit criteria: two simultaneous clients with different `TERM` values both look
right, a server started with stdout redirected to `/dev/null` still gives
colour to a 256-colour client, and `go vet` shows no use of the removed globals.

## Phase 4: container and sidecar deployment (1 day)

Delivers the first deployable shape: no native Prometheus needed, no change to
how `omtui-dash` talks to its backend.

### 4a. In-cluster scrape config (0.5 day)

1. `internal/managed/scrape.go`: add a direct (in-cluster) variant next to
   `GenerateScrapeConfig`. `ScrapeParams` gets `InCluster bool`; with it,
   - `kubernetes_sd_configs` omits `api_server`, so service discovery uses the
     ServiceAccount token and CA from the pod;
   - `__address__` becomes `<pod_ip>:<port>` and `__metrics_path__` is the plain
     path, instead of the `/api/v1/namespaces/.../pods/...:port/proxy` rewrite;
   - the Running-phase filter, namespace and selector handling, annotation mode,
     and the `namespace`, `pod` and `instance` labels stay exactly as they are,
     so queries in a dashboard behave the same in both modes.
2. `--print-scrape-config`: a new flag, mutually exclusive with everything
   except the dashboard path and `--namespace`. It prints the in-cluster config
   to stdout and exits 0. This is the source for the ConfigMap, so it must not
   need a backend or a cluster.
3. Tests: golden files for the in-cluster output across the same cases the
   proxy variant has (default, selector, fixed port, annotation mode, namespaces),
   plus one test that the two variants differ only in addressing and path labels.
4. Verify against real VictoriaMetrics with the existing fake Kubernetes API
   from the managed tests, pointed at it as `api_server` (the config cannot
   omit it outside a cluster). Real in-cluster discovery is verified in 4c.

### 4b. Image and manifests (0.25 day)

1. `Dockerfile` for `omtui-dash` only: multi-stage, static `CGO_ENABLED=0`,
   distroless or scratch base, non-root user. No `kubectl` and no
   `victoria-metrics` in it.
2. `examples/ssh/` (kustomize or plain YAML):
   - `Deployment`, one replica, two containers: `victoriametrics/victoria-metrics`
     (pinned tag, `-promscrape.config=/scrape/scrape.yml`,
     `-retentionPeriod=24h`, `-search.latencyOffset=0`, storage on an
     `emptyDir`) and `omtui-dash` (`--prom http://localhost:8428 --ssh-listen
     :2222 --ssh-host-key /keys/host --ssh-authorized-keys
     /auth/authorized_keys /dash/dashboard.yaml`);
   - `ConfigMap`s for the dashboard and the generated scrape config (a short
     Makefile target regenerates the latter with `--print-scrape-config`);
   - `Secret`s for the SSH host key and the authorized keys;
   - `ServiceAccount`, and a `Role`/`RoleBinding` (or `ClusterRole` for all
     namespaces) allowing `list` and `watch` on `pods`. No `pods/proxy`, which
     is the point of scraping directly;
   - `Service` (ClusterIP by default, LoadBalancer as a commented option);
   - TCP readiness probe on the SSH port, and an HTTP probe on VictoriaMetrics'
     `/health`.
3. Resource requests and limits from a short test at the session cap.

### 4c. Verify on a real cluster (0.25 day)

Run the example on the kind setup from `docs/kubernetes-local.md` with the demo
workloads: the sidecar discovers and scrapes the pods, panels fill, two SSH
sessions work independently, and a pod restart keeps the host key (Secret) but
loses history (`emptyDir`, documented). Managed mode already works against a
real cluster; this step checks the new in-cluster discovery, which differs in
how targets are addressed.

Exit criteria: `kubectl apply -k examples/ssh` followed by `ssh -p 2222 <svc>`
shows live data from a cluster that has no Prometheus.

## Phase 5: dashboard catalog (1 day)

Lets one deployment serve several dashboards, chosen per connection. It needs the
Phase 2 handler, and the Phase 4 example is updated to mount a directory.

1. **Catalog (0.25 day).** `internal/dashboard/catalog.go`:
   - `LoadCatalog(dir) (*Catalog, error)` reads `*.yaml` and `*.yml` in `dir`
     with `Load`, sorted by name; `Catalog.Names()`, `Catalog.Get(name)`.
   - Names must match `^[a-z0-9][a-z0-9._-]*$` after stripping the extension;
     other files are skipped with a warning. Lookup is a map access, so
     `../x` or an absolute path can never reach the filesystem.
   - A second file with the same name (`a.yaml` and `a.yml`) is an error.
   - `Reload()` re-reads the directory; a file that fails to parse is skipped
     and reported, the rest stay usable. Startup treats any failure as fatal.
2. **Wiring (0.25 day).** `dashapp.go`:
   - The path argument may be a directory. A file is a catalog of one, so the
     single-dashboard behaviour is unchanged.
   - Directory mode requires `--prom`, rejects `--var`, `--check` is allowed (it
     checks every dashboard), and warns when a dashboard sets `datasource.url`.
   - With `--print-scrape-config`, exactly one dashboard may carry `scrape:`;
     more than one is an error, none uses the defaults.
   - The SSH handler reloads the catalog per connection and builds the session
     from the chosen dashboard and its own default `Values`.
3. **Selection (0.5 day).**
   - Name from `sess.Command()`: exactly one argument, otherwise an error
     message. `ssh -t host NAME`.
   - No name: one dashboard opens directly; several show a picker, a small
     Bubble Tea model (list of title and name, arrows, Enter, `q` to quit) that
     returns the chosen dashboard. It renders through the session renderer from
     Phase 3.
   - Unknown name: print the available names to the session and exit non-zero.
   - Nothing is user-written into a path or a query: the name only selects an
     already-validated dashboard.

Tests:
- catalog: listing and order, invalid file skipped on reload but fatal at
  startup, name rules, duplicate name, `../x` and absolute paths not found,
  empty directory is an error;
- `dashapp`: directory with `--var` rejected, without `--prom` rejected, two
  `scrape:` sections rejected with `--print-scrape-config`;
- SSH integration: `NAME` opens that dashboard, unknown name lists the names,
  no name with two dashboards shows the picker and Enter opens one, a file added
  to the directory appears on the next connection.

Exit criteria: a deployment with two dashboards in one ConfigMap; `ssh -t host
team-a` and `ssh -t host team-b` each open their own, a plain `ssh host` shows the
picker, and editing the ConfigMap changes what new connections see.

## Phase 6: documentation (0.25 day)

- `README.md`: a short "Serve over SSH" section, flags, the auth model and the
  security notes (public-key only, no shell, session cap, host key from a
  Secret so clients do not see key changes).
- `docs/kubernetes-local.md`: link to the example.
- README: the catalog directory, `ssh -t host NAME`, the picker, and the catalog
  rules (needs `--prom`, no `--var`, one `scrape:` section).
- `docs/kubernetes-local.md`: the sidecar deployment as a fourth way to run it,
  and `--print-scrape-config`.
- State the limits plainly: no snapshot key, the sidecar means no `R` restart or
  `L` log keys (the UI does not manage VictoriaMetrics), history is lost on pod
  restart unless a PVC is used, the headless HTTP endpoint is not available over
  SSH, colour depth depends on the client.

## Phase 7 (later): single image with managed mode over SSH (2 days)

Only worth doing if the two-container pod is a problem, for example because you
want one image to ship, or the health banner, `L` logs and `R` restart in the
UI. It reuses the in-cluster scrape config from Phase 4a.

1. **In-cluster managed mode (0.75 day).** `--managed --in-cluster` (or
   detected from `KUBERNETES_SERVICE_HOST`):
   - skip `kubectl` startup and its preflight (`kubectl version`,
     `auth can-i`); start only VictoriaMetrics with the in-cluster scrape config
     from `GenerateScrapeConfig{InCluster: true}`;
   - `Health`, `Logs`, `Restart` and `Events` keep working for the one child.
     `Restart` no longer needs both children to restart together, since there is
     no proxy port to change;
   - `--kubectl-bin`, `--kube-context` are rejected with `--in-cluster`.
   Existing managed (proxy) mode is unchanged.
2. **Share the managed backend between sessions (0.5 day).** Two problems today:
   - `Backend.Events()` is one channel read by each session's `waitEvent`, so a
     child exit reaches one session only. Add a broadcaster that fans events out
     to every subscriber, and have `dashtui` subscribe per model; unsubscribe on
     session end so nothing leaks.
   - `Restart` from two sessions at once: serialise it, and make a second call
     while one is running return the same result instead of starting another.
   Then relax the Phase 2 validation to allow `--ssh-listen` with `--managed
   --in-cluster`; `--managed` without `--in-cluster` stays rejected, because it
   would use the server's kubeconfig for everyone who connects.
3. **Image (0.25 day).** A second Dockerfile target (or a build arg) based on a
   small distro image that carries a pinned `victoria-metrics` binary next to
   `omtui-dash`, still without `kubectl`. Run as non-root with a writable
   `--data-dir` (`emptyDir` or PVC).
4. **Manifests and verification (0.5 day).** `examples/ssh-single/`: the same as
   `examples/ssh/` with one container, `--managed --in-cluster`, and a PVC
   variant. Run it on kind. Kill the VictoriaMetrics child and check the banner
   shows in two sessions at once and `R` recovers both.

Tests: event fan-out (two subscribers both receive; an unsubscribed one does not
block the broadcaster), concurrent `Restart`, scrape config selected by
`--in-cluster`, flag validation, and the SSH integration test with a fake
managed backend.

Exit criteria: one image, one container, `ssh` shows data, a deliberate child
failure is visible and recoverable from any session.

## Effort and order

| Phase                                       | Estimate       | Depends on |
| ------------------------------------------- | -------------- | ---------- |
| 0 Groundwork and spike                      | 0.25 day       |            |
| 1 Session split                             | 0.5 day        | 0          |
| 2 SSH server                                | 0.75 day       | 1          |
| 3 Per-session colours                       | 1.5 days       | 2          |
| 4 Container and sidecar deployment          | 1 day          | 2          |
| 5 Dashboard catalog                         | 1 day          | 2, 3       |
| 6 Documentation                             | 0.25 day       | 3, 4, 5    |
| **Subtotal, first release (option C)**      | **~5.25 days** |            |
| 7 Single image, managed over SSH (option B) | ~2 days        | 2, 4a      |

Phases 3, 4 and 5 are independent once Phase 2 lands (5 uses the Phase 3 renderer for its picker), and 4a (the scrape config
variant) has no dependency on SSH at all, so it can start in parallel from day
one. Shipping after Phase 2 gives a working but colour-unreliable server, so I
would not release before Phase 3.

## Risks

| Risk                                                    | Mitigation                                                                                          |
| ------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| Wish v1 needs a newer Bubble Tea or lipgloss            | Phase 0 catches it before any code is written.                                                      |
| Missed global styles leak the server's colour profile   | Remove the globals so the compiler finds them; the `TERM=dumb` integration test catches the rest.   |
| `textinput` or other bubbles use the default renderer   | Audit in Phase 0; set their styles from the session renderer.                                        |
| Open server with `--ssh-allow-any-key`                  | Explicit flag, warning at startup, session cap; documented as "only for non-sensitive metrics".      |
| Many viewers multiply queries against the backend       | Session cap now; shared query cache as a follow-up.                                                  |
| In-cluster scrape config differs from the proxy variant | 4c runs it on kind with the demo workloads; golden tests cover the generated config.                |
| Pods not reachable from the scraper's pod (network policy, host-network pods) | Document it; the proxy variant remains available for local `--managed` use. |
| History lost when the sidecar pod restarts              | `emptyDir` by default, documented; PVC variant in Phase 7 manifests, and the same applies to option C if wanted. |
| Admin dashboards carry a `datasource.url` that differs from `--prom` | Ignored with a startup warning; the catalog always uses the one shared `Querier`.                    |
| A bad dashboard in the ConfigMap breaks new connections | Invalid files are skipped on reload and logged; startup is strict.                                   |
| Event fan-out bugs in Phase 7 (leaks, blocked broadcaster) | Non-blocking sends with a small buffer per subscriber, unsubscribe on session end, tests for both. |
| Host key regenerated on every pod restart               | Document mounting it from a Secret; log the fingerprint at startup.                                  |

## Follow-ups (not in this plan)

- User-supplied dashboards. Options are a paste box in the TUI (ephemeral, no
  storage) or `scp` into a per-key directory (persistent, needs a PVC). Either
  must ignore `datasource.url` and `scrape:`, cap size, panel count and refresh
  rate, and reuse the catalog's validation.
- Switching dashboards inside a session without reconnecting.
- Shared query cache so N sessions on the same dashboard and
  variables issue one query per interval.
- Idle timeout and per-key session limits.
- Choosing the dashboard or variables from the SSH command (`ssh host -- ...`).
- SSH certificate authentication.
