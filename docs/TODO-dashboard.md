# TODO: PromQL dashboard (`omtui-dash`)

Checklist derived from [PLAN-dashboard.md](PLAN-dashboard.md).

## Phase 0: restructure repo (one commit, no behaviour change)

- [x] Create `cmd/omtui/main.go` as thin entry point calling `internal/app`
- [x] Move model/Update/View/flags to `internal/app`
- [ ] `git mv` independent files: `store`, `fetch`, `filter`, `histogram`, `header`
- [x] Decide on `aggregate` (stays in `app` or small interface; it has `model` methods)
- [x] Export only names that cross package boundaries
- [x] Move tests with their files
- [x] `Makefile`: `BINARY_NAME=omtui`, build `./cmd/omtui`, update `clean`/`run`
- [x] `flake.nix`: `pname`, `mainProgram`, `subPackages = [ "cmd/omtui" ]`, refresh `vendorHash`
- [x] `README.md`: install command and binary name in examples (note the breaking rename)
- [x] `README-AGENT-CONSUMERS.md` and `.gitignore`: binary name
- [x] Verify `make build test lint` and `nix build`; no Go code left in repo root

## Phase 1: Prometheus API client (`internal/promapi`)

- [x] `Querier` interface (`QueryRange`, `Instant`), `Series`, `Sample` types
- [x] HTTP client: POST form, timeout, context cancellation
- [x] Auth: bearer token file, basic auth user + password file (client only; flags wired in Phase 2)
- [x] Parse `status/errorType/error`; handle `NaN`/`+Inf`; `matrix`, `vector`, `scalar`
- [x] Step selection helper (`max(minStep, range/width)`, under 11k points)
- [x] `httptest` tests: success, error payload, non-2xx, malformed JSON, timeout, `NaN`

## Phase 2: Dashboard definition (`internal/dashboard`, `cmd/omtui-dash`)

- [x] Create `cmd/omtui-dash` entry point with `--check`, `--var`, `--prom`, managed flags (stubs)
- [x] YAML types (dashboard, panel, variables, `scrape:`) with `yaml.v3`, `KnownFields(true)`, line numbers via `yaml.Node`
- [x] `windows` offsets, `agg`, `unit`, `thresholds`, `label` template, `sort`, `limit`, `folded`
- [x] Variables: `default`, `options`, `pattern`, `regex`; max nine; escape for PromQL string literal
- [x] Validation with line-numbered errors (required fields, unknown keys, window syntax, thresholds, label template, duplicate titles, unused variable warning)
- [x] Offline PromQL syntax check via `promql/parser` (`Validate(query) error`)
- [x] `--check`: run every query, per-panel status, non-zero exit on failure
- [x] Golden YAML fixtures, valid and invalid
- [x] Confirm Appendix A example validates

## Phase 3: Pure logic

- [x] Selector extraction (`Selectors(query)`) with placeholder + `parser.Inspect`; tests for regex, multiple selectors, nested, unparseable, cascading
- [x] Window extraction (columns, `agg`, step alignment, gaps, `NaN`, earliest sample time)
- [x] Threshold evaluation (`gt/gte/lt/lte`, catch-all)
- [x] Label templates with `{k=v,...}` fallback
- [x] Unit formatting (`percent`, `s`, `/min`, plain), copied helpers
- [x] Sort and limit
- [x] Folded panel summary (colour counts, worst row, ties, no thresholds, empty)
- [x] Sparkline rendering (resample, flat, empty)

## Phase 4: Managed backend (`internal/managed`)

- [x] Verify VictoriaMetrics assumptions: min `-retentionPeriod`, `-search.latencyOffset` effect (24h minimum, bare number = months; default offset hides fresh samples, `=0` passed; see [managed-verification.md](managed-verification.md))
- [ ] Verify Appendix B scrape config items (duplicate targets, `api_server` over HTTP, `__tmp_path` rules, `labeldrop`, port fallback). Verified against real VictoriaMetrics + a fake API; still open: real `kubectl proxy` watches, TLS/non-HTTP pods
- [x] Mode selection (`--prom`, `--managed`, `datasource.url`, error listing options)
- [x] Managed flags: `--kube-context`, `--namespace`, `--kubectl-bin`, `--vm-bin`, `--data-dir`, `--retention`, `--scrape-config`, `--log-file`
- [ ] Preflight (`kubectl version --client`, `auth can-i` checks) (tested with fakes only; never run against a real kubectl)
- [ ] Start `kubectl proxy --port=0`, parse port, `GET /livez`, stdin closed (implemented, tested with a fake proxy; not run against a real `kubectl proxy`)
- [x] Scrape config generator (annotation and selector modes), 0700 temp dir
- [x] Start VictoriaMetrics with port retry, `GET /health`
- [x] Lifecycle: process groups, `Pdeathsig`, ordered shutdown (SIGTERM, 5s, SIGKILL, cleanup)
- [x] Bounded log ring buffer, child watch, restart support
- [x] `victoria-metrics -version` minimum check (accept devbox 1.156.0 and Nix)
- [x] Startup failures exit before TUI with failing step and output tail
- [x] Tests: `TestHelperProcess` supervisor tests, golden scrape config, optional VM + mock-server integration test
- [x] Packaging: `flake.nix` `makeWrapper`, `--suffix PATH`, `packages/apps.omtui-dash`, dev shell `victoriametrics`, `vendorHash`

## Phase 5: TUI (`cmd/omtui-dash`)

- [ ] Bubbletea model (panels, cursors, active panel, error state)
- [ ] Refresh loop per panel, partial failures, back-off, generation counter
- [ ] Layout with shared column grid, scrolling, narrow-width degradation
- [ ] Folding (per panel, `z` all, live summaries, error on folded line)
- [ ] Header bar with variable boxes (`1`-`9`), shared `textinput`, validation in footer
- [ ] Suggestion overlay (substring filter, 8 rows, "N more", "k of n values", cached lookups)
- [ ] Keys: tab, up/down, space/enter, `s`, `/`, `r`, `p`, `+`/`-`, `?`, `q`, `L`, `R`
- [ ] Footer in `Name (key)` style; per-panel threshold legend
- [ ] Status line (title, backend mode, child health); cold-start and "no data (not scraped?)" notes
- [ ] Snapshot-to-file
- [ ] Tests: golden view strings at fixed widths, update loop with fake `Querier`

## Phase 6: Mock backend and demo

- [ ] `cmd/mock-server`: `/api/v1/query_range` and `/api/v1/query` with synthetic matrices
- [ ] `examples/controllers.yaml` working against the mock
- [ ] screenshots in `docs/`

## Phase 7: Documentation

- [ ] README section: YAML reference, keys, flags, backend modes
- [ ] `docs/kubernetes-local.md`: managed mode, RBAC, cold start, load caveats, manual recipe, `--prom` with port-forward
- [ ] Makefile targets: `build`, `dash`, `run-dash`

## Checkpoints

- [ ] Phase 0 merged, tests green
- [ ] Phases 1-3 merged: `omtui-dash --check` works against a real backend
- [ ] Phase 4 merged: managed `--check` leaves no processes; `nix run .#omtui-dash` works without installed `victoria-metrics`
- [ ] Phase 5 merged: usable against mock, external, and managed
- [ ] Phases 6-7 merged: demo, screenshots, docs
