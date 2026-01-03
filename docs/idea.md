# openmetrics-tui

goal: a tool to monitor prometheus/openmetrics metrics and displaying them in
the terminal. Metrics should be displayed using a simple, one-line text format
for each metric. A table format should be used to show older values, with the
most recent value at the right side and values from prior metric polls to the
left.

```
         t-3   t-2  t-1  recent
metric_1  32    34   36      38
metric_2   2    10    8      20
```

We should use golang and charmbracelet table widget for the table.

The table width should dynamically be adjusted to fit the terminal window width
taking into account the width of he data elements. Older columns not fitting
inside the terminal window should not be shown.

Metrics should be pulled from a single endpoint and the tool needs to parse the
prometheus/openmetrics format. The endpoint will be given to the program as a
command line argument. Old metrics needs to be stored such that the table can
show historical data. The polling interval and the amount of historical data to
store (measured in poll iterations) should be defined using command line
arguments.

Metrics labels should not be show by default, but command line arguments should
be available to 1) show all labels in the table and 2) an regexp to define which
metrics to show and 3) regexp filter on labels, e.g. "show metrics with label
foo=bar

A `--show-deltas` command line argument should change the metric show to be the
delta from the previous sample instead of the absolute value.
