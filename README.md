# openmetrics-tui

A terminal-based tool to monitor OpenMetrics/Prometheus metrics in real-time. A
simple alternative to `watch 'curl http://... | grep metric_*`' workflows.

Metrics view:

![Screenshot-metrics](docs/screen1.png)

Distributions view:

![Screenshot-distributions](docs/screen2.png)

Scrape epochs, after the target dropped out and came back (different colour for
each epoc):

![Screenshot-epochs](docs/screen3.png)

Handing the screen to an AI agent? Press `S` to save a plain-text snapshot - see
[Plain-text snapshots](#plain-text-snapshots) below - and pass it along with
[README-AGENT-CONSUMERS.md](README-AGENT-CONSUMERS.md), which explains the
columns, colors and footer in detail.

## Filtering and aggregation

Metrics can be filtered on metric name and labels and be aggregated on labels.

Each of the three takes the same syntax on the command line and in the TUI, so
the examples below show the value alone.

### Metric filter

An unanchored regex matched against the metric name. Pass it as
`-filter-metric`, or press `m` to edit it live (`enter` applies, `esc`
discards).

```
http_requests
^go_(gc|memstats)
requests_total|errors_total
```

### Label filter

A comma-separated list of clauses, all of which must match. Pass it as
`-filter-label`, or press `f` to edit it live. A missing label reads as the
empty string, the way PromQL treats it, so `env!=dev` also admits series with no
`env` label.

```
env=prod            label equals a value
env!=dev            label does not equal a value
env=~prod|stg       label matches a regex
env!~dev|test       label does not match a regex
code=               series lacking the label
env!=               series carrying the label, whatever its value
5..                 bare regex, tried against every label value (not its name)
env=prod,code=~5..  all clauses must match
path=~/a{1,2}       commas inside {} or [] need no escaping; use \, elsewhere
```

### Aggregation

Combines series that differ only in the labels you name. Pass it as
`-aggregation`, or press `a` to edit it live. The operator is optional and
defaults to `sum`; grouping never crosses metric names.

```
pod                 sum series that differ only in pod
pod,instance        sum over both labels
avg pod             avg, min, max and count also work
count               no grouping labels: one row per metric
sum by (pod)        the PromQL spelling, if you prefer it
```

Histograms fold with `sum` only; summaries are never folded and pass through
untouched.

### Label display

A label the filter pins to a value is dropped from the `{}` braces, because the
header already shows the filter and every visible series implicitly match. Under
`env=prod,code=~5..` a row reads `http_requests_total{method=GET}`, not
`{code=503,env=prod,method=GET}`.

A clause that pins nothing leaves its label alone: `env!=dev` admits prod and
staging both, so the column still says which one this row is. The same goes for
`env!~dev|test` and for a bare regex, which names no label to begin with.

Press `l` to cycle, or set `-label-mode`.

```
hide-filtered       the default described above
hide-all            no labels at all
all                 every label shown, filter or no filter
```

## Processing of metrics

The display has two independent axes. Deltas run _across time_, along a row, and
apply to every metric. Bucket mode runs _within a single sample_, down a
histogram's bounds. Neither knows about the other, so any pairing of the two
means what both names say it means, and both are shown at the bottom of the
screen.

### Deltas across time

Toggled with `d`, or set with `-delta-mode`.

- `Deltas: Off` - metrics show raw.
- `Deltas: Δ Next` - metrics columns show the delta to the next in the time
  series, except for the current column, which show the raw metric value. _This
  makes changes over time easier to observe_.
- `Deltas: Δ View` - the current column shows the delta within the current view,
  i.e. the newest value less the oldest one still on screen, which is the sum of
  the deltas in the columns beside it.

A counter that falls has been restarted rather than measured, so the drop reads
as missing (`.`) instead of a large negative number. A gauge is free to fall,
and keeps its negative delta.

### Buckets within a sample

Toggled with `b`, in the distributions view. A histogram's buckets are
cumulative as exported Prometheus/OpenMetric data, i.e. `le=0.5` counts
everything `le=0.1` already counted.

- `Buckets: Cumulative` - counts exactly as the exporter published them, rising
  as you read down towards `+Inf`.
- `Buckets: Per bucket` - each bucket less the one below it, so a row counts
  only the observations that fell in that band. _This makes 'peaks' in the data
  easier to observe_.

Either way the counts are lifetime totals; pair them with `d` to see what
arrived recently. Summary quantiles are latencies rather than counts, so bucket
mode leaves them alone.

A two-bucket histogram, scraped three times, under each pairing:

```
                              t-2  t-1  Curr        t-2  t-1  Curr
Deltas: Off                    10   14    20         10   14    20   le=0.1
                               30   40    55         20   26    35   le=+Inf
Δ Next                          4    6    20          4    6    20   le=0.1
                               10   15    55          6    9    35   le=+Inf

                          Buckets: Cumulative    Buckets: Per bucket
```

## Scrape epochs

A failed scrape marks a break in the history, and the next successful scrape
starts a new _epoch_. That usually means the target restarted, so the values on
either side of the break come from two different processes. They shouldn't be
read as one continuous series.

Columns from before the most recent break are drawn in muted colours:
grey-lavender for raw values and muted orange for deltas. The newest epoch
always keeps the normal colours. If there's more than one break in the retained
history, the shading alternates at each one going back. In the
[epochs screenshot](docs/screen3.png), the column-header ages also jump from
`-20s` to `-10s`, which is where the break happened.

Deltas never cross a break, whatever the metric kind:

- In `Δ Next`, the column right before the break gets no delta, because the two
  samples next to it belong to different epochs.
- In `Δ View`, the current column only covers the newest epoch. It shows growth
  since the reconnect, not the two processes' counts added together.

## Plain-text snapshots

Press `S` (shift-s; lowercase `s` is the unrelated hide-static toggle) to save
the current view to `openmetrics-tui-snapshot-<timestamp>.txt` in the working
directory. This is the preferred way to hand a screen to an AI agent instead of
an image screenshot - it carries exact numbers with no risk of OCR misreading
them.

A snapshot captures whichever view is on screen - the Metrics table or the
Distributions accordion - under whatever filters, aggregation, delta mode and
bucket mode are currently active, with colour escape codes stripped.

Unlike an actual screenshot, it is not clipped to the terminal's height: it
lists every row the current filters admit, as if the terminal were infinitely
tall. Width is still capped, though - the number of history or grid columns is
limited to whatever the terminal was wide enough to show at the time, exactly as
on screen. If a distribution family was zoomed (`enter` pressed twice), the
snapshot captures only that family full-screen, since zoom is a deliberate focus
rather than a height limitation.

The file opens with a short preamble:

```
# openmetrics-tui snapshot, taken 2026-09-30T15:57:37Z
# Colour is stripped from this file - see README-AGENT-CONSUMERS.md
# for how to read it without it.
```

That timestamp is the wall-clock moment the snapshot was taken - the column
headers inside the file are still relative ages (`-45s`, `Curr`, ...), measured
from it. The footer briefly echoes `saved <path>` (or a `snapshot
failed: ...`
error) in place of `? for help`, clearing itself again after a few seconds.
