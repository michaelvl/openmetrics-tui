# openmetrics-tui

A terminal-based tool to monitor OpenMetrics/Prometheus metrics in real-time using a dynamic table layout. Built with Go and [Bubble Tea](https://github.com/charmbracelet/bubbletea).

## Features

- **Real-time Monitoring**: Polls metrics endpoints at configurable intervals.
- **Dynamic Layout**: Automatically adjusts columns based on terminal width.
- **Historical Data**: View historical trends with configurable history size.
- **Filtering**: Filter metrics by name and labels using Regex.
- **Deltas**: Option to view value changes (deltas) instead of absolute values.
- **Mock Server**: Includes a mock server for testing and development.

## Installation

### Build from Source

Requirements: Go 1.21+

1. Clone the repository:
   ```bash
   git clone https://github.com/michaelvl/openmetrics-tui.git
   cd openmetrics-tui
   ```

2. Build the binary:
   ```bash
   make build
   ```

   This will create the `openmetrics-tui` binary in the current directory.

## Usage

Basic usage requires providing the URL of the metrics endpoint:

```bash
./openmetrics-tui -url http://localhost:9090/metrics
```

### Command Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-url` | URL to poll metrics from (required) | - |
| `-interval` | Polling interval | `5s` |
| `-history` | Number of historical samples to keep | `10` |
| `-show-labels` | Show all labels in the table | `false` |
| `-filter-metric` | Regex to filter metrics by name | - |
| `-filter-label` | Regex to filter metrics by label | - |
| `-show-deltas` | Show deltas instead of absolute values | `false` |

When `-show-deltas` is enabled:
- The **Curr** (rightmost) column shows the **absolute value**.
- Historical columns (e.g., `T-1`) show the **delta** (change) leading to the next time step.
  - `T-1` shows `Value(Curr) - Value(T-1)`
  - `T-2` shows `Value(T-1) - Value(T-2)`

### Key Bindings

- **q** / **ctrl+c**: Quit the application.
- **↑** / **↓**: Navigate the table rows.

## Examples

**Monitor a local Prometheus instance:**
```bash
./openmetrics-tui -url http://localhost:9090/metrics
```

**Filter for specific metrics (e.g., HTTP requests):**
```bash
./openmetrics-tui -url http://localhost:9090/metrics -filter-metric "http_requests_.*"
```

**Show deltas with a faster polling interval:**
```bash
./openmetrics-tui -url http://localhost:9090/metrics -interval 1s -show-deltas
```

## Mock Server

The project includes a mock server that serves sample metrics for testing purposes.

1. Build the mock server:
   ```bash
   make mock-server
   ```

2. Run the mock server (defaults to port 8080):
   ```bash
   ./mock-server -port 8080
   ```

3. Run the TUI against the mock server:
   ```bash
   ./openmetrics-tui -url http://localhost:8080/metrics
   ```

## Development

- **Build all binaries**: `make all`
- **Run tests**: `make test`
- **Lint code**: `make lint`
- **Clean**: `make clean`
