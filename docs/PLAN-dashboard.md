# Plan: PromQL dashboard command (`omtui-dash`)

## Goal

A second TUI (`omtui-dash`), next to the existing single-endpoint viewer
(`omtui`), that renders a dashboard defined in YAML. Each panel is a PromQL
query. Each returned series is a row, columns are time windows (now, 5m, 15m,
1h), cells are coloured by thresholds, and a sparkline shows the trend. The data
source is the Prometheus HTTP API, so VictoriaMetrics or Prometheus does the
scraping, Kubernetes discovery, storage and PromQL evaluation.

The backend is chosen at startup:

- **External:** point the tool at an existing Prometheus or VictoriaMetrics API
  (`--prom http://...`), for example one reached through `kubectl port-forward`
  or running in-cluster.
- **Managed:** the tool starts `kubectl proxy` and a local VictoriaMetrics as
  child processes, wires VictoriaMetrics to scrape pods through the proxy, and
  tears both down on exit. No backend needs to be installed in the cluster.

Non-goals: our own TSDB, our own PromQL engine, our own Kubernetes discovery
(`kubectl` and VictoriaMetrics do that), changing the behaviour of the existing
viewer.

## Decisions

| Topic                | Decision                                                                                                                                                |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Separate command     | Yes, `cmd/omtui-dash`, same repo and module.                                                                                                            |
| Existing tool        | Renamed to `omtui`, entry point moves to `cmd/omtui`. The old `go install` path and the `openmetrics-tui` binary name are dropped (breaking, accepted). |
| Backend              | Any Prometheus-compatible HTTP API. VictoriaMetrics single-node is the documented default.                                                              |
| PromQL parsing       | `github.com/prometheus/prometheus/promql/parser` is a dependency, used only to parse and inspect queries (syntax checks, variable suggestions), not to evaluate them. |
| Embedding            | None. Talk REST (`/api/v1/query_range`, `/api/v1/query`).                                                                                               |
| Backend modes        | External (`--prom URL`) or managed (`--managed`: child `kubectl proxy` + child VictoriaMetrics). Mutually exclusive; the TUI code only sees a `Querier`. |
| Windows              | One `query_range` per panel; columns are picked from that response, instant value at `t-window` by default, optional `agg: avg\|max\|min`.              |
| Thresholds           | Evaluated per cell, first match wins. A `●` marker accompanies the colour so it is not the only signal.                                                 |
| `rate()` intervals   | Written explicitly in the query. No `$__interval` in v1.                                                                                                |
| Shared helpers       | Copy the few formatting helpers at first; extract to `internal/ui` only once the overlap is proven.                                                     |
| Local cluster access | Managed mode automates `kubectl proxy` plus a local VictoriaMetrics with a pod-proxy scrape config; the same recipe is documented for doing it by hand. No Kubernetes client code in our binaries. |
| Managed binaries     | Looked up on `PATH` (`kubectl`, `victoria-metrics`), overridable by flag. No auto-download in v1; a clear error with an install hint instead. `kubectl` is user-managed and never bundled (keeps us independent of Kubernetes versions). The Nix package wraps `omtui-dash` so `victoria-metrics` is on `PATH` (see Phase 4, Packaging).           |

## Phase 0: restructure the repo into folders

The repo is currently one flat `package main` (about 8k lines) in the root. Do
this first, as its own change with no behaviour change, so the dashboard work
lands in a sensible layout.

Target layout:

```
cmd/
  omtui/                     main.go: thin entry point, calls internal/app
  omtui-dash/                created in Phase 2 (`--check`), TUI added in Phase 5
  mock-server/               unchanged
internal/
  app/                       today's main.go: model, Update, View, flags
  store/                     store.go (series, epochs, scrape ring)
  fetch/                     fetcher.go
  aggregate/                 aggregate.go
  filter/                    filter.go
  histogram/                 histogram.go, histogram_view.go
  header/                    header.go
docs/
```

There is no Go code in the repo root afterwards.

Notes and decisions to make while doing it:

- The existing tool is renamed `omtui`. This is a breaking change:
  `go install github.com/michaelvl/openmetrics-tui@latest` stops working and
  becomes `go install github.com/michaelvl/openmetrics-tui/cmd/omtui@latest`,
  and the `openmetrics-tui` binary name goes away.
- Package boundaries follow current coupling. `aggregate.go` has methods on
  `model` (`m.aggregateSeries`), so those either stay in `app` or get a small
  interface. Do not force a clean split where the code is genuinely entangled; a
  first pass that moves only the clearly independent files (`store`, `fetch`,
  `filter`, `histogram`, `header`) and leaves model-coupled code in `app` is
  acceptable.
- Exported names: types currently lowercase and shared across files (`model`,
  `distEntry`, `cursorSpan`, ...) will need exporting only where they cross a
  package boundary. Keep the surface small.
- Tests move with their files. `go test ./...` must pass before and after, and
  the diff should be mostly renames (`git mv`) plus package clauses and import
  edits.

Checklist of files that reference the old layout or name:

- `Makefile`: `BINARY_NAME=omtui`; `build` becomes
  `go build -o $(BINARY_NAME) ./cmd/omtui`; `mock-server` target unchanged;
  `clean` and `run` use the new name.
- `flake.nix`: `pname`, `mainProgram = "omtui"`, `subPackages = [ "cmd/omtui" ]`
  (add `cmd/omtui-dash` in Phase 2; the VictoriaMetrics wrapper lands in
  Phase 4, see Packaging); refresh `vendorHash` if the build complains.
- `README.md`: install command, binary name in all usage examples.
- `README-AGENT-CONSUMERS.md`: binary name, if mentioned.
- `.gitignore`: built binary name.

Acceptance: `make build test lint`, `nix build`. One commit.

## Phase 1: Prometheus API client (`internal/promapi`)

- `Querier` interface:
  `QueryRange(ctx, expr, start, end, step) ([]Series, error)` and
  `Instant(ctx, expr, at) ([]Series, error)`.
- `Series{Labels map[string]string; Points []Sample}`,
  `Sample{T time.Time; V float64}`.
- HTTP client: POST form-encoded, request timeout, context cancellation,
  optional auth (`--prom-token-file FILE` for a bearer token, or basic auth
  from `--prom-user` and `--prom-password-file`; never on the command line),
  parse
  `status/errorType/error`, handle `NaN` and `+Inf` strings, handle `matrix`,
  `vector` and `scalar` result types.
- Step selection helper: `step = max(minStep, (end-start)/width)` to stay well
  under the 11,000 point limit.
- Tests with `httptest`: success, error payload, non-2xx, malformed JSON,
  timeouts, `NaN`.

## Phase 2: Dashboard definition (`internal/dashboard`)

See Appendix A for the example YAML.

- Create `cmd/omtui-dash` here as a thin entry point with `--check`, `--var`,
  `--prom` and the managed flags (Phase 4 fills in the latter). The TUI is
  added in Phase 5.
- YAML via `gopkg.in/yaml.v3`: decode into `yaml.Node` first for line numbers,
  then into the types with `KnownFields(true)` so unknown keys are errors.
- YAML types: dashboard (title, datasource, refresh, windows, spark, variables,
  panels) and panel (title, query, label template, unit, thresholds, sort,
  limit, agg, folded).
- Schema decisions (settled; see Appendix A):
  - `windows` is a single list of offsets, `now` first (`[now, 5m, 15m, 1h]`).
    A column's value is the instant at `end - offset`, snapped to the nearest
    step. Per-panel `agg: avg|max|min` replaces that, for columns other than
    `now`, with the aggregate over the trailing window `[end-offset, end]`;
    default is no `agg`.
  - `unit: percent` multiplies the value by 100 and adds `%`; thresholds
    always compare the raw value. Other units: `s`, `/min`, none.
  - `thresholds` is an ordered list, first match wins; an entry with no
    comparison is the catch-all. Comparisons: `gt`, `gte`, `lt`, `lte`.
  - `label:` supports only `{{labelname}}` substitutions, falling back to
    `{k=v,...}`.
  - A variable that no panel query uses is a validation warning.
- Optional `scrape:` section used only in managed mode: `namespaces`,
  `interval`, and either `selector` (a pod label selector) or `annotation`
  opt-in, plus `port` and `path`. In annotation mode, `annotation`, `port`
  and `path` hold annotation names; the generator converts `prometheus.io/port`
  into the relabel meta label `__meta_kubernetes_pod_annotation_prometheus_io_port`
  (every character outside `[A-Za-z0-9_]` becomes `_`). In selector mode,
  `port` is a number (default: the first declared container port; pods
  without one are skipped) and `path` a literal (default `/metrics`). See
  Phase 4.
- Variables: a map of name to `{default, options, pattern, regex}`. Names are
  referenced in queries as `${name}` and shown in the UI as `$name`. Each gets
  an editable header box with a number key assigned by declaration order
  (`1` to `9`; more than nine is a validation error). `default` is the
  initial value (`--var name=value` overrides it); `pattern` validates edits
  before they are substituted (the substituted value is also escaped for a
  PromQL string literal, so `\` and `"` can never break out of the matcher),
  with a safe default of `^[A-Za-z0-9_.:/-]*$` that
  only allows regex metacharacters when `regex: true`. Suggested values are
  derived automatically from the queries (see Phase 3, selector extraction):
  a matcher such as `namespace="${namespace}"` or `controller=~"${controller}"`
  names the label and the enclosing selector, and the values come from
  `/api/v1/label/<label>/values?match[]=<selector>`. The matcher for the
  variable being edited is dropped from the selector (otherwise the list would
  be limited to the current value) and other variables are substituted with
  their current values, so lists cascade ($controller only offers controllers
  seen in the chosen $namespace). An explicit `options:` list of strings
  overrides the derivation, `options: {label_values: {metric: m, label: l}}`
  names the lookup explicitly, and `options: false` disables it. A failed or empty lookup falls
  back to free text. Variables only affect queries; window set and
  refresh interval are dashboard settings, not variables.
- Validation with line-numbered errors: required fields, unknown keys, window
  syntax, threshold shape, label template syntax, duplicate titles, and PromQL
  syntax of every query, checked offline with `promql/parser` after substituting
  variable defaults, so `--check` can report a typo without a backend.
- `--check`: load the YAML, run every query once, print per-panel status (rows
  returned, errors). Exit non-zero on failure. This is useful in CI and before
  launching the TUI.
- Tests with golden YAML fixtures, valid and invalid.

## Phase 3: Pure logic

All testable without a terminal.

- Selector extraction (`internal/dashboard`, uses
  `github.com/prometheus/prometheus/promql/parser`): replace each `${name}` in
  a query with a placeholder token, parse, and walk the AST with
  `parser.Inspect`. For every `VectorSelector` with a label matcher whose value
  is the placeholder, record the variable, the label name, the matcher type
  (`=`, `=~`) and the selector (metric name plus the remaining matchers). Output
  is the list of `(variable, label, selector)` used to build the
  `label_values` lookups. Variables in positions that do not parse with a
  placeholder (for example inside a range duration) yield no derivation and
  fall back to free text. Tests: single and multiple selectors, regex matchers,
  a variable used in several queries, nested aggregations, binary operations,
  unparseable queries, and cascading substitution.

- Window extraction: from one `[]Series` response, compute each column (value at
  `now`, `now-5m`, ... or `agg` over the window). The query range is
  `[now - max(windows, spark), now]` and the step is chosen so every offset
  lands on a step boundary where possible (`step` divides the smallest nonzero
  offset); otherwise the nearest step is used. `now` is the last non-`NaN`
  point. Handle gaps, series that appear or disappear mid-range, and stale
  `NaN` points. Also report the earliest sample time per series, which the
  cold-start note (Phase 4) uses.
- Threshold evaluation: value to colour name, with `gt`, `gte`, `lt`, `lte` and
  a catch-all.
- Label templates: `{{controller}}` rendering, with a fallback to `{k=v,...}`.
- Unit formatting: `percent`, `s`, `/min`, and plain numbers, using copies of
  the existing tool's formatting helpers (see Decisions, shared helpers).
- Sort and limit.
- Panel summary for folded panels: count of rows per threshold colour (computed
  on the `now` column) and the worst row. Severity order is red, then yellow,
  then green; unknown or custom colours rank lowest. Ties go to the highest
  value. Panels without thresholds show the row count; a single-series panel
  shows just its value. Tests: ties, mixed colours, no thresholds, `NaN`-only
  series, empty results.
- Sparkline rendering: resample points to N cells, map to block characters, with
  explicit handling for flat and empty series.

## Phase 4: Managed backend (`internal/managed`)

Lets the tool run its own backend against the current kubeconfig context, so a
dashboard works with nothing installed in the cluster.

Selecting the mode:

- `--prom URL`: external backend, nothing is spawned.
- `--managed`: spawn children (below). `datasource.url` in the YAML is ignored.
- Neither flag: use `datasource.url` from the YAML, or fail with a message that
  lists both options.
- Managed-only flags: `--kube-context`, `--namespace` (overrides
  `scrape.namespaces`), `--kubectl-bin`, `--vm-bin`, `--data-dir` (default: a
  temp dir removed on exit; if given, kept so history survives restarts),
  `--retention` (default 24h; VictoriaMetrics rejects less, to be verified), `--scrape-config FILE` (escape hatch, see below).

What gets started, in order:

0. Preflight, before the TUI takes the terminal: `kubectl version --client`,
   then `kubectl auth can-i get pods --subresource=proxy [--context=...]` and
   `kubectl auth can-i list pods`. Any interactive login happens here, in the
   normal screen. Failures name the missing step or RBAC permission.
1. `kubectl proxy --port=0 [--context=...]`, bound to 127.0.0.1. The chosen
   port is parsed from its "Starting to serve on 127.0.0.1:PORT" line, then
   readiness is confirmed with a `GET /livez`. `kubectl` handles all cluster
   authentication, including exec plugins (EKS, GKE, AKS), so we never touch
   credentials. Stdin is closed so an auth plugin cannot hang on a prompt.
2. A generated scrape config (see Appendix B) in a 0700 temp dir. It points
   `kubernetes_sd_configs.api_server` at the proxy, and rewrites each target to
   `/api/v1/namespaces/$ns/pods/$pod:$port/proxy$path` with `__address__` set to
   the proxy (the recipe from the earlier design discussion), and sets
   `namespace`, `pod` and `container` labels. It is generated from the `scrape:`
   section of the dashboard YAML. `--scrape-config` supplies a user-written
   file instead; `${KUBE_PROXY}` in it is replaced with `127.0.0.1:PORT`.
3. `victoria-metrics -promscrape.config=... -storageDataPath=...
   -httpListenAddr=127.0.0.1:PORT -retentionPeriod=...`. The port is picked by
   binding `:0` and releasing it; if VictoriaMetrics fails to bind, retry with a
   new port a few times. Readiness is `GET /health`.

The `Querier` from Phase 1 is then created with the VictoriaMetrics URL; the TUI
does not know which mode it is in.

Lifecycle:

- Children run in their own process group. On Linux set `Pdeathsig` so they die
  if the TUI is killed hard; on other platforms fall back to best effort.
- Shutdown (quit, SIGINT, SIGTERM, panic via `defer`): stop in reverse order,
  SIGTERM, wait up to 5s, then SIGKILL, then remove the temp dir.
- Child stdout and stderr are captured into a bounded ring buffer and never
  written to the terminal (it would corrupt the alt-screen). The TUI shows them
  in a log overlay (`L`) and writes them to `--log-file` if set.
- Children are watched. If one exits unexpectedly the TUI shows a banner with the
  last log lines and offers `R` to restart, while panels show the error state
  from Phase 5.
- A startup failure (missing binary, `kubectl` unauthenticated, no RBAC for
  `pods/proxy`, port clash) exits before the TUI opens, with the failing step
  and the tail of its output.

Behaviours to document and surface in the UI:

- **Cold start.** A fresh storage dir has no history, so a 1h window is empty
  until an hour has passed. The tool detects this from the earliest sample time
  per series (Phase 3): a column whose offset reaches before it shows `·`, and
  the panel title shows "collecting data (N min)". Panels show "collecting data (N min)" instead of
  blanks. `--data-dir` persists history across runs, and panels fill in
  immediately on the next start.
- **Variables never change the scrape scope.** `scrape.namespaces` decides what
  is collected; a `$namespace` variable only filters queries. Pointing it at a
  namespace that is not scraped yields empty panels, so panels show "no data
  (not scraped?)" in that case, and managed mode defaults `scrape.namespaces` to
  all namespaces so variables act as filters. Regenerating and reloading the
  scrape config when a variable changes is a possible later feature.
- **Load.** Every scrape goes through the API server. The default scrape
  interval is 15s, with a documented warning for large pod counts.
- **Version.** Check `victoria-metrics -version` at startup and warn below a
  documented minimum. `kubectl` is not version-checked beyond running; it is the
  user's own and should follow the Kubernetes skew policy for their cluster
  (within one minor version), which the docs state.

Dependencies:

- `kubectl` is a user-managed dependency and is not bundled anywhere. This keeps
  us independent of Kubernetes versions: the user's `kubectl` matches their
  cluster, and we only rely on the long-stable `kubectl proxy` interface. If it
  is missing, startup fails with a clear message and an install hint.
- VictoriaMetrics is bundled in the Nix package (below) and otherwise looked up
  on `PATH`.

Packaging (lands with the managed backend in this phase; the `omtui-dash` binary exists since Phase 2):

- `flake.nix` makes VictoriaMetrics a runtime dependency of `omtui-dash`, so
  `nix run` and `nix profile install` only need `kubectl` on the machine. Add
  `nativeBuildInputs = [ pkgs.makeWrapper ]`, include `cmd/omtui-dash` in
  `subPackages`, and in `postInstall` run
  `wrapProgram $out/bin/omtui-dash --suffix PATH : ${lib.makeBinPath [ pkgs.victoriametrics ]}`.
  `--suffix` (not `--prefix`) means a `victoria-metrics` already on the user's
  `PATH` wins over the bundled one; `--vm-bin` overrides both. Only
  `omtui-dash` is wrapped; `omtui` has no runtime dependencies.
- The flake also exposes `packages.omtui-dash` and `apps.omtui-dash`, in
  addition to `omtui` and the default.
- The dev shell adds `pkgs.victoriametrics` next to `gnumake` and
  `golangci-lint`, matching `devbox.json`, where VictoriaMetrics is already
  pinned (`victoriametrics@1.156.0`). `kubectl` is not added; developers use
  their own.
- Version alignment: Nix follows `nixpkgs-unstable` while devbox pins 1.156.0.
  The minimum version check in the startup preflight must accept both, and CI
  should note which one it exercised.
- The new YAML dependency changes `go.sum`, so refresh `vendorHash`.
- The integration test needs `victoria-metrics` on `PATH`. Inside the Nix build
  sandbox it is not, so the test skips there unless `victoriametrics` is added
  to `checkInputs`; decide whether the extra build time is worth it (default:
  leave it out and run the integration test from the dev shell and CI).

Tests:

- First task of the phase: verify the VictoriaMetrics assumptions below against
  a real binary: the minimum accepted `-retentionPeriod`, and whether
  `-search.latencyOffset` (default 30s) hides the newest samples so that the
  `now` column lags; if it does, managed mode passes `-search.latencyOffset=0`.
- Process supervisor tested with the `TestHelperProcess` pattern (a re-exec'd
  test binary acting as a fake `kubectl` or VM that prints the ready line, hangs,
  crashes, or ignores SIGTERM), covering ordered startup, readiness timeouts,
  crash detection, escalation to SIGKILL, and cleanup of the temp dir.
- Scrape config generation tested against golden files.
- An optional integration test, skipped when `victoria-metrics` is not on
  `PATH`, that starts VictoriaMetrics against `cmd/mock-server` as a plain static
  target (no cluster) and queries the result.

## Phase 5: TUI (`cmd/omtui-dash`)

See Appendix C for the layout reference.

- Bubbletea model: panels, per-panel row cursor, active panel, window set,
  last-refresh status, per-panel error state.
- Refresh loop: one goroutine-safe command per panel per tick, partial failures
  shown on the panel without blanking the others. Back-off on repeated errors.
- Layout: stacked panels with a shared column grid, scrolling when the terminal
  is short, graceful narrow-width behaviour (drop the sparkline first, then the
  longest window).
- Folding: a panel is either expanded (title row, column header, rows) or
  folded to one line: `▸ title`, per-colour row counts and the worst row (see
  Appendix C). Expanded panels are marked `▾`. Initial state comes from the
  panel's `folded:` field and toggles are kept for the session. Folded panels
  keep querying so the summary stays live; only rendering changes. A folded
  panel in error shows the error text on its line. A fully folded dashboard is
  the brief overview.
- Header: a fixed 3-row bar like the existing tool's, containing only variable
  boxes, titled `$name (n)` where `n` is the number key that opens it. One
  shared `textinput`, Enter applies, Esc cancels, Tab moves between boxes, and a
  validation error goes in the footer so the bar never changes height. While a
  box is open, a suggestion list overlays the top of the panel area (so the bar
  keeps its height), filtered by substring as the user types: `↑`/`↓` select,
  Tab or Enter accept the highlighted value, at most 8 rows with an "N more"
  line, and the footer shows "k of n values". Typed text not in the list is
  still accepted if it passes `pattern`, with a "not in list" note. Values are
  fetched when the box opens and cached briefly. Applying a value re-queries all
  panels and cancels in-flight requests with a generation counter, as the
  existing tool does for ticks. Boxes shrink to short captions on narrow
  terminals and then overflow into a popup.
- Keys (mirroring the existing tool where it makes sense): `1` to `9` edit
  variables, tab or shift-tab for panels, up/down for rows, space or enter to
  fold or unfold the active panel, `z` to fold or unfold all, `s` to cycle
  sort (the YAML `sort` order, then `now` descending, `now` ascending, label),
  `/` to filter rows (while the filter or a variable box is open, all keys go
  to the input, including `1` to `9`), `r` refresh now, `?` help, `q` quit.
  `+`/`-` change the refresh interval and `p` pauses, as in the existing tool.
  The window columns and refresh interval are shown in the status line, not in
  header boxes; windows come from the YAML only. The footer uses the existing tool's `Name (key)` segments.
- Footer with key hints in the existing tool's `Name (key)` style. The
  threshold colour legend is per panel and sits on each panel's title row.
- The status line next to the variable boxes shows the dashboard title, the
  backend mode (`external <url>` or `managed <context>`) and, in managed mode, a
  child-process health indicator; `L` opens the log overlay and `R` restarts a
  failed child.
- Snapshot-to-file, as in the existing tool.
- Tests: view rendering against golden strings at fixed widths, update-loop
  tests with a fake `Querier`.

## Phase 6: Mock backend and demo

- Extend `cmd/mock-server` with `/api/v1/query_range` and `/api/v1/query` that
  return synthetic matrices for a fixed set of queries, so the dashboard can be
  developed, tested and screenshotted without a cluster.
- Example `examples/controllers.yaml` that works against the mock.
- A `vhs` tape and screenshots in `docs/`, as for the existing tool.

## Phase 7: Documentation and local-cluster recipe

- README section for the new command: YAML reference, key bindings, flags, and
  the two backend modes.
- `docs/kubernetes-local.md`: managed mode as the primary path (prerequisites:
  `kubectl` installed and authenticated by the user, within one minor version
  of the cluster; `victoria-metrics`, bundled with the Nix package; required RBAC `pods/proxy` plus list/watch on
  pods; cold start and `--data-dir`; load caveats). Also the manual recipe
  (`kubectl proxy` plus a hand-run VictoriaMetrics with a pod-proxy scrape
  config) and the in-cluster alternative with `kubectl port-forward` to an
  existing backend, used via `--prom`.
- Makefile targets: `build`, `dash`, `run-dash`. Nix flake changes are described
  under Phase 4, Packaging.

## Later, only if needed

- Extract shared formatting and style helpers to `internal/ui`.
- `$__interval`-style variables, per-panel step override.
- Alert-style panel summary (worst row colours the title).
- Multi-target direct scraping in the existing viewer (`Scraper` and
  `MultiScraper`), independent of this work.
- A second panel type (full-width time-series chart).
- Auto-download of a pinned VictoriaMetrics release for managed mode.
- Managed mode for plain Prometheus (agent mode) as an alternative child.

## Risks

- **Restructure churn (Phase 0).** `model`-coupled helpers resist clean package
  splits. Mitigation: move the independent files first, accept that some code
  stays in `app`, and keep the change behaviour-neutral.
- **Breaking rename.** The `omtui` rename and new install path break existing
  users of the old name. Mitigation: do it once, in Phase 0, and note it in the
  README.
- **Query load.** Many panels at a short refresh can stress a backend.
  Mitigation: one request per panel, a sensible default refresh, and back-off on
  errors.
- **`pods/proxy` limits.** Scraping through the API server is fine for ad-hoc
  use but not for large clusters. The docs should say so.
- **Orphaned children (Phase 4).** A hard kill of the TUI can leave
  `kubectl proxy` or VictoriaMetrics running. Mitigation: process groups,
  `Pdeathsig` on Linux, signal handlers and `defer` cleanup, plus a startup check
  that fails clearly if the chosen data dir is locked by a stale instance.
- **Port races.** A port picked by bind-and-release can be taken before the
  child binds it. Mitigation: retry with a new port; read the actual port from
  `kubectl proxy` output instead of choosing it.
- **Auth prompts.** `kubectl` may want interactive login (SSO, exec plugins).
  Mitigation: run a `kubectl auth can-i` style preflight before the TUI takes
  over the terminal, so any login happens in the normal screen, and close stdin
  on the children.
- **Platform differences.** `Pdeathsig` is Linux-only, and VictoriaMetrics
  binaries differ per platform. Mitigation: Linux and macOS are supported
  targets first; Windows is untested until asked for.
- **Parser dependency.** `prometheus/prometheus` is a large module, and the
  parser package follows its (v0.x tagged) releases. Only the parser and its
  small transitive set are compiled, but `go.sum` and the Nix `vendorHash` grow,
  and parser upgrades can change the AST API. Mitigation: confine all use to one
  package behind a small `Selectors(query) []Selector` function and a
  `Validate(query) error` function, pin the version, and keep tests that exercise
  them so an upgrade breaks loudly.
- **Version drift in the API.** The Prometheus HTTP API is stable, and
  VictoriaMetrics-specific features (relative `start=-1h`) are avoided.

## Order and checkpoints

1. Phase 0 merged, all tests green, no behaviour change.
2. Phases 1 to 3 merged: `omtui-dash --check` works against a real backend.
3. Phase 4 merged: `omtui-dash --managed --check` starts and stops both
   children cleanly against a real cluster and leaves no processes behind;
   `nix run .#omtui-dash -- --managed --check` works on a machine that has
   `kubectl` but no `victoria-metrics` installed.
4. Phase 5 merged: usable dashboard against the mock, an external backend and
   managed mode.
5. Phases 6 and 7 merged: demo, screenshots and docs.

## Appendix A: Example dashboard YAML (design sketch)

Defines the schema that Phase 2 validates and Phase 3 consumes. It becomes
`examples/controllers.yaml` in Phase 6. Not final; see the open questions below.

```yaml
# controllers.yaml
title: Controller health
datasource:
  url: http://localhost:8428 # external mode; overridden by --prom, ignored by --managed
refresh: 10s
windows: [now, 5m, 15m, 1h] # columns, single list, now first; one query_range per panel feeds all of them
spark: 1h # sparkline range

variables: # shown as $namespace and $controller; keys 1 and 2 by declaration order
  namespace:
    default: prod # override with --var namespace=staging
    # suggestions are derived from namespace="${namespace}" in the panel queries;
    # override with options: [prod, staging] or
    # options: { label_values: { metric: kube_pod_info, label: namespace } }
    pattern: '^[a-z0-9-]+$' # checked before substitution
  controller:
    default: ".*" # "all" is a regex, so panels use controller=~"${controller}"
    regex: true # allows regex metacharacters in the value

# Used only in managed mode (Phase 4); generates Appendix B.
scrape:
  namespaces: [prod]
  interval: 15s
  annotation: prometheus.io/scrape # opt-in; omit to scrape every Running pod with a port
  port: prometheus.io/port # annotation holding the metrics port
  path: prometheus.io/path # annotation holding the path (default /metrics)

panels:
  - title: Reconciles per resource per minute
    query: |
      sum by (controller) (rate(controller_runtime_reconcile_total{namespace="${namespace}", controller=~"${controller}"}[5m])) * 60
      /
      on (controller) group_left
      count by (controller) (kube_customresource_info{namespace="${namespace}", controller=~"${controller}"})
    # kube_customresource_info is user-defined via kube-state-metrics
    # custom-resource-state config; the mock backend (Phase 6) serves it.
    unit: "/min"
    label: "{{controller}}" # row title; one row per series
    thresholds: # per cell, first match wins
      - { gt: 5, color: red }
      - { gt: 1, color: yellow }
      - { lte: 1, color: green }

  - title: Reconcile error ratio
    query: |
      sum by (controller) (rate(controller_runtime_reconcile_errors_total{namespace="${namespace}", controller=~"${controller}"}[5m]))
      / sum by (controller) (rate(controller_runtime_reconcile_total{namespace="${namespace}", controller=~"${controller}"}[5m]))
    unit: percent # value x100, "%" suffix
    label: "{{controller}}"
    thresholds:
      - { gt: 0.05, color: red }
      - { gt: 0.01, color: yellow }
      - { color: green }

  - title: Workqueue depth
    query: max by (name) (workqueue_depth{namespace="${namespace}"})
    label: "{{name}}"
    thresholds:
      - { gt: 100, color: red }
      - { color: green }

  - title: Pod restarts (1h)
    query: sum by (pod) (increase(kube_pod_container_status_restarts_total{namespace="${namespace}"}[1h]))
    label: "{{pod}}"
    sort: desc # by the "now" column
    limit: 8 # top-N rows
    thresholds:
      - { gt: 0, color: red }
      - { color: green }

  - title: API server p99 latency
    query: histogram_quantile(0.99, sum by (le) (rate(apiserver_request_duration_seconds_bucket[5m])))
    unit: s
    thresholds:
      - { gt: 1, color: red }
      - { gt: 0.3, color: yellow }
      - { color: green }
```

Decisions taken (also in Phase 2) and what remains:

- `windows` is a single list of offsets; no per-column objects in v1.
- `unit: percent` multiplies by 100; thresholds compare the raw value.
- `thresholds` is an ordered list, first match wins.
- `label:` supports only `{{labelname}}`.
- Default `agg` is none (instant value at the column offset).
- The reconcile-ratio panel keeps its division, which assumes a user-defined
  `kube_customresource_info` series (kube-state-metrics custom-resource-state
  config). The docs give a non-dividing variant for clusters without it. Still
  to confirm in Phase 2 when writing the golden fixtures: that the example
  validates and renders against the mock.

## Appendix B: Example generated scrape config (design sketch, untested)

What `internal/managed` should generate from the `scrape:` section of
Appendix A in annotation mode, with the `kubectl proxy` port filled in (shown as `127.0.0.1:8001`). Selector mode replaces the two annotation `keep`/port rules with a `__meta_kubernetes_pod_label_*` keep rule and the first container port (or the configured one), see Phase 2.
It is the golden file for the Phase 4 generator tests. It was written from
documentation and memory and has not been run against VictoriaMetrics or a
cluster; verifying it is the first task of Phase 4, especially the items under
"To verify" below.

```yaml
scrape_configs:
  - job_name: pods-via-proxy
    scrape_interval: 15s
    scheme: http
    kubernetes_sd_configs:
      - role: pod
        api_server: http://127.0.0.1:8001 # discovery through kubectl proxy
        namespaces:
          names: [prod]
    relabel_configs:
      # only running pods that opted in
      - source_labels: [__meta_kubernetes_pod_phase]
        action: keep
        regex: Running
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        action: keep
        regex: "true"
      # path: annotation or /metrics
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_path]
        regex: (.+)
        target_label: __tmp_path
      - source_labels: [__tmp_path]
        regex: ""
        replacement: /metrics
        target_label: __tmp_path
      # build the API server pod-proxy path; port comes from the annotation
      - source_labels:
          - __meta_kubernetes_namespace
          - __meta_kubernetes_pod_name
          - __meta_kubernetes_pod_annotation_prometheus_io_port
          - __tmp_path
        regex: (.+);(.+);(.+);(.+)
        replacement: /api/v1/namespaces/$1/pods/$2:$3/proxy$4
        target_label: __metrics_path__
      # every target is reached through the proxy
      - target_label: __address__
        replacement: 127.0.0.1:8001
      # series labels
      - source_labels: [__meta_kubernetes_namespace]
        target_label: namespace
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
      - source_labels: [__meta_kubernetes_pod_container_name]
        target_label: container
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: instance # otherwise every target shares instance=127.0.0.1:8001
      - regex: __tmp_path
        action: labeldrop
```

To verify in Phase 4:

- Targets that differ only in `__metrics_path__` and `instance` are not merged
  or dropped as duplicates by VictoriaMetrics.
- `api_server` accepts the plain-HTTP proxy address for pod discovery, and
  watches work through `kubectl proxy`.
- The `__tmp_path` default rule behaves as intended (empty source label matches
  `regex: ""`), and the `labeldrop` removes it.
- Pods with several container ports and pods without the port annotation: decide
  whether to fall back to the first declared container port or skip the pod.
- `pods/proxy` scrapes of pods using TLS or non-HTTP metrics ports.

## Appendix C: Example TUI view (illustrative)

Layout reference for Phase 5. All numbers are invented. In a real terminal the
value cells are coloured by the panel thresholds; here the `●` marker carries
the same information so colour is not the only signal. Rows that would be red:
`database-controller` (7.9/min, 9.1% errors) and the `gadget` queue (143).
Yellow: `gadget-controller` at 3.2/min and 1.4% errors.

```
 ┌ $namespace (1) ───────┐┌ $controller (2) ──────┐
 │ prod                  ││ .*                    │  Controller health  managed ctx=dev  last ok 14:02:11
 └───────────────────────┘└───────────────────────┘  windows now 5m 15m 1h  ⟳ 10s (+/-)
 ─────────────────────────────────────────────────────────────────────────────────────────
 ▾ Reconciles per resource per minute (/min)                      ●<1  ●1-5  ●>5
 ▌                                    now      5m      15m      1h     trend (1h)
 ▌ widget-controller                  0.4●     0.5     0.4      0.6    ▂▃▃▂▂▃▂▂▃▃▂▂
 ▌ gadget-controller                  3.2●     2.8     1.1      0.9    ▁▁▂▂▃▄▅▆▆▇▇▇
 ▌ database-controller                7.9●     8.1     7.7      6.2    ▄▅▅▆▆▆▇▇▇▇▇▇
 ▌ ingress-controller                 0.0●     0.0     0.1      0.1    ▁▁▁▁▁▁▁▁▁▁▁▁
 ─────────────────────────────────────────────────────────────────────────────────────────
 ▾ Reconcile error ratio                                          ●<1%  ●1-5%  ●>5%
 ▌                                    now      5m      15m      1h     trend (1h)
 ▌ widget-controller                  0.0%●    0.0%    0.0%     0.0%   ▁▁▁▁▁▁▁▁▁▁▁▁
 ▌ gadget-controller                  1.4%●    1.2%    0.3%     0.1%   ▁▁▁▁▁▂▂▃▄▅▆▆
 ▌ database-controller                9.1%●    8.7%    8.8%     8.5%   ▇▇▇▇▇▇▇▇▇▇▇▇
 ─────────────────────────────────────────────────────────────────────────────────────────
 ▾ Workqueue depth                     now      5m      15m      1h     trend (1h)
 ▌ widget                             2●       1       1        0      ▁▁▁▁▁▁▂▁▁▁▂▂
 ▌ gadget                             143●     120     40       3      ▁▁▁▁▂▂▃▄▅▆▇█
 ─────────────────────────────────────────────────────────────────────────────────────────
 ▾ Pod restarts (1h), top 8            now      5m      15m      1h     trend (1h)
 ▌ database-0                         3●       3       2        0      ▁▁▁▁▁▃▃▃▅▅▅▅
 ▌ gadget-6f9c-x2k                    1●       1       0        0      ▁▁▁▁▁▁▁▁▅▅▅▅
 ─────────────────────────────────────────────────────────────────────────────────────────
 ▾ API server p99 latency (s)          now      5m      15m      1h     trend (1h)
 ▌ (single series)                    0.21●    0.19    0.20     0.22   ▃▃▃▃▃▃▃▃▃▃▃▃
 ─────────────────────────────────────────────────────────────────────────────────────────
  ? for help | ⟳ 10s (+/-) | Fold (space) | Sort (s) | Log (L) | ● ok
```

While editing a variable (key `1` pressed), the box gets the cursor and the
footer shows the edit hint or a validation error. The bar stays 3 rows high.
When suggestions are available, a list overlays the top of the panel area.

```
 ┌ $namespace (1) ───────┐┌ $controller (2) ──────┐
 │ prod                  ││ gad▏                  │  Controller health  managed ctx=dev
 └───────────────────────┘└───────────────────────┘  editing $controller
                          ┌───────────────────────┐
                          │ ▸ gadget-controller   │
                          │   widget-gadget-sync  │
                          │   … 3 more            │
                          └───────────────────────┘
  ↑↓ select  Tab/Enter accept  Esc cancel │ 2 of 7 values
```

```
 ┌ $namespace (1) ───────┐┌ $controller (2) ──────┐
 │ stag▏                 ││ .*                    │  Controller health  managed ctx=dev
 └───────────────────────┘└───────────────────────┘  editing $namespace
  Enter apply  Esc cancel  Tab next  ↓ options │ invalid: $namespace must match ^[a-z0-9-]+$
```

Cold start in managed mode (Phase 4): until enough history exists, columns that
reach further back than the stored data show a placeholder instead of a value,
and the panel title carries a note.

```
 ▾ Reconciles per resource per minute (/min)        collecting data (3 min)
 ▌                                    now      5m      15m      1h     trend (1h)
 ▌ widget-controller                  0.4●     ·       ·        ·      ▂▃▃
```

Folding (Phase 5): a panel collapses to one line with per-colour row counts
and the worst row. Mixed, with the first panel expanded:

```
 ▾ Reconciles per resource per minute (/min)                     ●<1  ●1-5  ●>5
 ▌                                    now      5m      15m      1h     trend (1h)
 ▌ widget-controller                  0.4●     0.5     0.4      0.6    ▂▃▃▂▂▃▂▂▃▃▂▂
 ▌ gadget-controller                  3.2●     2.8     1.1      0.9    ▁▁▂▂▃▄▅▆▆▇▇▇
 ▌ database-controller                7.9●     8.1     7.7      6.2    ▄▅▅▆▆▆▇▇▇▇▇▇
 ▌ ingress-controller                 0.0●     0.0     0.1      0.1    ▁▁▁▁▁▁▁▁▁▁▁▁
 ─────────────────────────────────────────────────────────────────────────────────────────
 ▸ Reconcile error ratio              ●1 ●1 ●1    worst: database-controller 9.1%
 ▸ Workqueue depth                    ●1 ●1       worst: gadget 143
 ▸ Pod restarts (1h), top 8           ●2          worst: database-0 3
 ▸ API server p99 latency (s)         ●1          0.21
```

Fully folded (`z`), the brief overview:

```
 ▸ Reconciles per resource per minute ●2 ●1 ●1   worst: database-controller 7.9/min
 ▸ Reconcile error ratio              ●1 ●1 ●1   worst: database-controller 9.1%
 ▸ Workqueue depth                    ●1 ●1      worst: gadget 143
 ▸ Pod restarts (1h), top 8           ●2         worst: database-0 3
 ▸ API server p99 latency (s)         ●1         0.21
```

In a real terminal each `●` is coloured, so a folded panel reads red, yellow or
green at a glance.

Design notes the mockup encodes:

- One request per panel feeds all window columns and the sparkline.
- Row identity is the label set; `label:` templates only change the display.
- A panel-level error (backend down, bad PromQL) replaces its rows with the
  error text, without blanking other panels.
- The header bar holds only variable boxes, so a `$` prefix means "variable"
  and `${name}` in a query matches what is on screen. Window set and refresh
  interval are dashboard settings, shown in the status line next to the boxes
  and changed with global keys.
- The threshold legend is per panel, on the panel's title row, because
  thresholds are defined per panel. The footer follows the existing tool's
  convention of `Name (key)` segments and carries no colour legend.
- The status line next to the boxes carries the dashboard title, the backend
  mode and, in managed mode, child-process health.
