# Reading openmetrics-tui as an agent

This document is for an AI agent that is handed a screenshot (or pasted text)
of an `openmetrics-tui` screen and asked to explain or reason about it, without
access to the running terminal, the command line it was started with, or the
raw `/metrics` endpoint. Read [README.md](README.md) first for what the
filter/aggregation/delta/bucket languages mean; this document is about how
those settings show up on screen, and — just as importantly — what a
screenshot cannot tell you.

The single most important rule: **absence is not evidence of a zero value.**
A metric, a label, a column, or an indicator can be missing from the screen
for several unrelated reasons — filtered out, aggregated away, hidden by
`hide-static`, not yet scraped, dropped for lack of terminal width, or simply
a mode the UI never displays at all. Each section below says which applies
where.

## Reading a colourless export

You are usually handed either a pasted-text copy of the terminal or the
tool's own saved snapshot (`S`, a plain-text export - see
[README.md](README.md#plain-text-snapshots) for how it's produced). Either
way, colour is unavailable. Everything elsewhere in this document about
columns, filters, labels, and what a `.` cell or an empty table means applies
identically without colour; none of that is colour-dependent. Only the
colour-coded cues below need a substitute:

| Colour cue (real terminal) | What to read instead, without colour |
|---|---|
| Magenta current value vs. plain historical value | The `Curr` column header already identifies which column is current |
| Orange delta text | The footer's `Deltas:` line states the mode, and every delta cell already carries an explicit `+`/`-` sign in the text itself |
| Grey-lavender / muted-orange "older epoch" shading | Look for an uneven jump in the column-header ages instead (see below) — that's the same underlying event (a connection break) that the colour was marking |
| Distribution heatmap shading (blue/cyan ramp) | No information is lost — every cell's exact number is printed either way; shading was only a shape-at-a-glance affordance, not the only source of the value |
| Highlighted cursor row (distributions view) | Not recoverable — plain text carries no indication of which row the cursor was on. Don't assume any particular row is "selected" |

If what you're reading is the tool's own saved snapshot rather than a raw
paste or an image, two further things follow from how it's produced: it
lists every row the current filters admit, not just what happened to fit on
screen at the time - so, unlike an image screenshot, never assume there's
more below that got cut off by scrolling - and its preamble timestamp, not
any timestamp in the table itself, is the "now" that the column headers'
relative ages (`-45s`, `Curr`, ...) are measured from.

## Layout, top to bottom

```
+-- header: three boxes --------------------------------------------------+
| Metric: <regex>  |  Label: <clauses>  |  Aggregation: <spec>            |
+-- viewport: Metrics table, or Distributions accordion ------------------+
| ...                                                                     |
+-- footer: one line -----------------------------------------------------+
| ? for help | v: Metrics | Deltas: ... | interval 5s | url    scroll: ^v |
+-------------------------------------------------------------------------+
```

The header is always exactly 3 rows and the footer always exactly 1 row; only
the middle area (the metrics table or the distributions accordion) scrolls or
changes height.

## The header boxes

Three boxes, always in this order: **Metric**, **Label**, **Aggregation**.
Each shows the filter/aggregation currently *applied* — not necessarily
what's mid-edit, since only one box is ever focused at a time and unfocused
boxes always reflect the live config.

- An empty box renders as a faint em dash `—`, meaning "no filter/aggregation
  set", not "empty string was typed".
- A focused box (only relevant if the screenshot was taken mid-edit) has a
  blue border, or a red border plus an error message in the footer's left
  segment if the typed value doesn't parse yet — in which case the *applied*
  filter is still the last valid one, not what's shown in the box.
- On a narrow terminal the caption itself may shrink (`Aggregation` →
  `Agg`) or disappear, and the value may be truncated with `…`. A truncated
  value in a box is not the full filter that's actually in effect.
- Syntax for each box's contents is in the README's "Filtering and
  aggregation" section. Briefly: **Metric** is a regex against the metric
  name; **Label** is comma-separated clauses (`env=prod`, `code=~5..`,
  a bare regex, etc.) all of which must match; **Aggregation** is an optional
  operator (`sum`, the default, `avg`, `min`, `max`, `count`) plus a
  comma-separated label list to group by.

**A screenshot with all three boxes empty means "show everything, raw,
ungrouped."** A screenshot with any of them filled changes what every row
below means — a filled Aggregation box in particular means the metric names
in the table are *already reduced* (see below), not raw series.

## The Metrics view (the table)

Rows are one per (metric name, label set) after filtering and aggregation are
applied. Columns are, left to right: **Metric** (name + labels), then one
column per retained scrape, oldest to newest, with the rightmost always
labeled **Curr**.

### Column headers are relative ages, not clock times

A header like `-45s` means "45 seconds before the newest (`Curr`) scrape",
not 45 seconds before now. If the ages between adjacent columns aren't even
(e.g. `-45s -40s -35s ... -12s -5s Curr` where the interval is nominally 5s),
a scrape was skipped or failed in that gap — most likely the target was
briefly unreachable, or the TUI was paused. This is legitimate: don't assume
uniform sampling.

Not every column is guaranteed to hold a real scrape either — see "Missing
values" below.

### Row identity depends on the header boxes

- **Label filter + default label mode (`hide-filtered`):** a label the filter
  pins to one value is *dropped from the row's `{}` braces*, because the
  header box already says what it's pinned to and every row implicitly
  matches. Under `Label: env=prod`, a row reading
  `http_requests_total{method=GET}` still has `env=prod` — it's just not
  repeated on every row. A clause that *doesn't* pin one value (`env!=dev`,
  a bare regex) leaves the label on the row, since rows can still differ on
  it.
- **Label mode is invisible on screen.** There is no footer or header
  indicator for which of the three label-display modes (`hide-filtered`
  default, `hide-all`, `all`) is active — it only changes which labels get
  printed on each row. If you need to know whether a label that isn't shown
  on a row genuinely doesn't apply, or is just hidden because it's pinned by
  the filter, cross-reference the **Label** box: a pinned label always
  applies to every row even when hidden.
- **Aggregation collapses rows.** If the Aggregation box is non-empty, each
  row is a *sum* (or avg/min/max/count) over every series that agrees on the
  grouping labels — the row is not a single exporter-reported series. A row
  named `sum(api_errors_total){method=get}` is the total across every other
  label combination for that method, and no longer decomposes into individual
  instances/pods/etc. Histograms fold under `sum` only; a non-`sum` operator
  or an aggregation spec applied where a summary is present leaves those
  families untouched (in the Distributions view this is called out
  explicitly — see below — the Metrics view has no equivalent note, so a
  summary or a histogram under a non-sum aggregation may sit in the table
  unaggregated even though the Aggregation box is filled).

### Cell values and colors

| Appearance | Meaning |
|---|---|
| Magenta text | The current (`Curr`) value, raw — shown when Deltas is Off or `Δ Next` |
| Orange text, with explicit `+`/`-` sign | A delta value (Deltas `Δ Next` or `Δ View`) |
| Grey-lavender text | A raw value from *before* the most recent connection break (see "Epochs" below) |
| Muted-orange text | A delta value from before the most recent connection break |
| Plain/default text | An older raw value in the current epoch, `Δ Next`/`Δ View` mode |
| `.` | No value for this cell — see "Missing values", not zero |

The delta mode governs what the numbers *mean*, and it's stated once in the
footer, not per-column — read it before interpreting any cell:

- **`Deltas: Off`** — every column is the raw value at that scrape.
- **`Deltas: Δ Next`** — every column except `Curr` is the delta to the next
  column in time; `Curr` alone is raw. This is the mode in `docs/screen1.png`.
- **`Deltas: Δ View`** — every column, including `Curr`, is a delta: the
  value at that scrape minus the value in the oldest column on screen. The
  columns still sum left-to-right to that total.

A negative delta for something that looks like a counter (monotonically
increasing) does not necessarily happen — see below.

### Missing values (`.`) versus a genuine zero

A `.` cell means the tool has no number to show there. Do not read it as `0`.
It happens when:

- The series hadn't started reporting yet at that scrape (fewer history
  samples than columns).
- The scrape at that column failed or was skipped (see column-age gaps
  above) and the connection error means no data is available for *any*
  series in that column.
- In a delta mode, the underlying counter's value *fell* between two
  samples — the tool treats that as the exporting process having restarted
  (a counter cannot legitimately decrease), so the delta is undefined and
  shown as `.` rather than a large, meaningless negative number. A *gauge*
  is allowed to fall and keeps its real (negative) delta instead.

### Epochs and connection breaks

Whenever a scrape fails, the tool marks a "break"; the next successful scrape
starts a new epoch. Columns from before the most recent break are drawn in
the alternate colors (grey-lavender / muted-orange, per the table above) so
a restarted target's before/after values don't read as one continuous
series. The newest epoch is always in the normal colors. A screenshot with
some columns in the alternate shade is evidence the target dropped and came
back at some point within the retained history window — the exact gap is
visible from the age jump in the column headers.

### A metric absent from the table entirely

If a metric you expected isn't in the table at all, it can be because:

1. It doesn't match the **Metric** or **Label** filter box (check both).
2. It was aggregated into a different, differently-named row (check the
   **Aggregation** box).
3. `Static: Hidden` is active in the footer (see below) and every one of its
   series has held a constant value for as long as history has been
   retained (fewer than 2 retained samples never counts as static, so a
   metric that just appeared is never hidden by this).
4. The exporter genuinely never published it in this scrape (nothing the TUI
   controls).
5. It's a histogram or summary, and you're looking at the Metrics view —
   those only ever appear in the **Distributions** view (see below). The
   Metrics table shows a hint line like "N distributions scraped — press v to
   see them" when this is the only reason the table looks emptier than
   expected.

When every row is hidden, the table says so explicitly, e.g. `No metrics to
display (all 12 metrics hidden by: metric filter, hide-static)` — trust that
message over inferring the reason yourself.

### How many history columns are actually shown

The number of columns is capped by `-history` (10 by default) but is further
cut to whatever fits the terminal width, dropping the *oldest* columns first
— `Curr` is never dropped. A screenshot showing fewer columns than you'd
expect from a stated `-history` value is not evidence of a shorter retention
window, just a narrower terminal (or a smaller window at the time).

## The Distributions view (histograms and summaries)

Switched to with `v` (footer reads `v: Distributions`); histograms and
summaries never appear in the Metrics view. Each family is one line:

```
DISTRIBUTION                              COUNT  RATE   p50    p90    p99
▸ http_request_duration_seconds{...}       6k    5.2/s  0.0621 0.912  >1
```

- The leading marker `▸` means collapsed, `▾` means expanded (its bucket/
  quantile grid is inlined directly beneath that line). Pressing `enter`
  again on an expanded family zooms it to fill the whole screen — a zoomed
  screenshot only shows one family, with no marker/list around it and an
  `esc: back` hint next to its title.
- **COUNT** and **RATE** are the family's total observation count and
  observations/sec since the previous scrape — always genuine cumulative
  totals, independent of whatever Delta or Bucket mode is set. A `.` in
  either means not enough samples yet, not zero traffic.
- **p50 / p90 / p99** are quantiles interpolated from the histogram's bucket
  boundaries (or read directly, for a summary that reports its own
  quantiles). A value prefixed `>` (e.g. `>1`) means that rank fell in the
  `+Inf` bucket — there's no finite upper bound to interpolate against, so
  the number shown is the last finite bucket bound, and the true value is
  only known to be *larger* than that. A `.` means not enough data to
  estimate that quantile at all.
- Labels on a distribution row follow the exact same label-mode and
  filter-pinning rules as the Metrics table (see above) — a label pinned by
  the **Label** box is dropped from the row's `{}` the same way.
- If the **Aggregation** box is filled, a one-line note appears next to the
  `N shown` count, e.g. `avg ignored: sum only` (histograms only fold under
  `sum`; other operators are silently not applied to distributions) or
  `summaries not summed` (a summary is never folded, regardless of
  operator, since its quantiles can't be combined arithmetically). Absence
  of that note when Aggregation is filled means the aggregation is not
  affecting anything shown — most likely every visible family is a
  histogram and the operator is `sum`.

### The bucket/quantile grid (expanded or zoomed)

Rows are bucket upper bounds (`le=...`, labeled `le`) for a histogram, or
reported quantile ranks (labeled `quantile`) for a summary. Columns are the
same time columns as the Metrics view, governed by the same footer-level
**Deltas** setting.

- **`Buckets: Cumulative`** (footer, distributions view only) — each row is
  exactly what the exporter published: bucket `le=1` includes everything
  `le=0.5` already counted, so values *increase* reading down the column
  towards `+Inf`.
- **`Buckets: Per bucket`** — each row has had the bucket below it
  subtracted off, showing only the observations that landed in that band.
  This is the tool's own transform, not what's on the wire.
- These are lifetime totals either way; pairing Bucket mode with a Delta
  mode shows recent arrivals per band instead of running totals. Summary
  rows (quantile values, not counts) are never affected by Bucket mode —
  the README's worked example under "Buckets within a sample" has exact
  numbers for a two-bucket case under every Delta × Bucket pairing.
- Blue/cyan cell shading is a heatmap, **scaled independently per family**
  (its own row of buckets, its own maximum) — brighter cells are busier
  buckets *for that family*, not a global intensity scale, so don't compare
  shade brightness across two different distributions. Only histograms are
  shaded; summary quantiles are latencies, not counts, so shading them would
  imply a comparison ("more traffic here") that doesn't apply. A cell with
  no shading and a `.` is a scrape gap in that column, same meaning as the
  Metrics table; a genuine zero-count bucket is unshaded but shows `0`.
  Columns from before a connection break use an alternate violet-tinted
  shading ramp, mirroring the Metrics view's alternate epoch colors.
- `native histogram - buckets not supported` in place of a grid means the
  family is a native (exponential) OpenMetrics histogram, which carries no
  classic bucket boundaries at all — this is a format limitation, not a
  filtering or empty-data state.

If nothing is shown at all, the empty message states why, e.g. `No
histograms or summaries to display (all 4 hidden by: label filter)` — as
with the Metrics view, trust that message over guessing.

## The footer

Left to right, some segments only appear conditionally:

| Segment | Meaning | When it appears |
|---|---|---|
| `? for help` | static hint | always, unless a header box is being edited (then this space shows the edit hint / validation error instead) |
| `v: Metrics` / `v: Distributions` | which top-level view is on screen | always |
| `Deltas: Off` / `Δ Next` / `Δ View` | the delta mode explained above | always — applies to *both* views |
| `Buckets: Cumulative` / `Per bucket` | the bucket mode explained above | only in the Distributions view, and only if terminal width allows (dropped first on a narrow terminal) |
| `⏸ PAUSED` | scraping is paused (`p`); the store stops accepting new samples but nothing on screen goes stale-colored for it | only while paused |
| `Static: Hidden` | `hide-static` is on (`s`): metrics/distributions whose retained values never changed are hidden entirely | only while active — its *absence* means hide-static is off, not "nothing is static" |
| `⟳ 5s` | the current scrape interval (`+`/`-` to change) | always, unless dropped for width on a very narrow terminal |
| `●` green + URL | connected; last scrape succeeded | while connected |
| `⚠` red + error text | not connected; shows the fetch error | while disconnected — the table still shows the last good data, now frozen, plus a new epoch will start on reconnect |
| `▲` / `▼` | there's more content above/below the current scroll position | only when the viewport isn't already at that edge, and never in a saved snapshot, which always contains every row |

Two things the footer (and header) never show, so cannot be read off a
screenshot:

- **The active label-display mode** (`hide-filtered` / `hide-all` / `all`) —
  infer it only from whether labels that the **Label** box pins are present
  or absent on the rows, per the "Row identity" section above.
- **The `-history` flag's configured value** — only however many columns
  currently fit are visible; a narrower terminal or window silently shows
  fewer than what's configured, oldest columns first (see above).

## Practical checklist for interpreting a screenshot

1. Read the three header boxes first — they change what every row name and
   grouping means, and a screenshot rarely calls this out on the rows
   themselves.
2. Read `Deltas:` before reading any number in the table or grid — the same
   digit means a totally different thing raw versus as a delta.
3. If in the Distributions view, also read `Buckets:` — it and `Deltas:` are
   independent axes and a cell only makes sense read against both.
4. Check for `Static: Hidden` and treat any specific metric's absence as
   possibly explained by it, alongside the two filter boxes.
5. Never conclude "this metric is zero" or "this metric doesn't exist" from
   it being absent from a screenshot — walk the reasons in "A metric absent
   from the table entirely" first, and prefer the tool's own "No metrics to
   display (...)" / "No histograms or summaries to display (...)" messages
   when they're visible, since they already state the reason.
6. A `.` cell is missing data for that one time column, never a zero.
7. Colors that aren't magenta/orange (i.e. the muted "epoch" shades) mean the
   data crosses a connection break — treat values on either side as separate
   runs, not one continuous series.
