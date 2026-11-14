package managed

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/managed/fakebin"
)

// TestHelperProcess is not a test: the test binary re-executes itself as a
// fake kubectl or victoria-metrics (see fakebin).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Exit(fakebin.Run(args[0], args[1:]))
}

// fakeOptions returns Options whose tools are the re-executed test binary.
func fakeOptions(t *testing.T) Options {
	t.Helper()
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("TMPDIR", t.TempDir()) // so leaked temp dirs are visible
	return Options{
		StartupTimeout: 5 * time.Second,
		ShutdownGrace:  500 * time.Millisecond,
		wrap: func(tool string, args []string) (string, []string) {
			return os.Args[0], append([]string{"-test.run=^TestHelperProcess$", "--", tool}, args...)
		},
	}
}

func argsFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_ARGS_FILE", f)
	return f
}

func leftovers(t *testing.T) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "omtui-dash-*"))
	return m
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return c
}
