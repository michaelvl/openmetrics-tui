package sshserve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/dashtui"
)

func catalogDash(title string) string {
	return "title: " + title + "\npanels:\n  - title: Panel of " + title + "\n    query: up\n"
}

// catalogServer serves a directory with two dashboards, team-a and team-b.
func catalogServer(t *testing.T) (dir string, c *gossh.Client) {
	t.Helper()
	dir = t.TempDir()
	for name, title := range map[string]string{"team-a.yaml": "Alpha", "team-b.yaml": "Beta"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(catalogDash(title)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := dashboard.LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	signer := newSigner(t)
	s := start(t, Config{AuthKeysPath: writeKeys(t, signer)}, CatalogModel(cat, dashtui.Options{Querier: emptyQuerier{}, NoSnapshot: true}))
	c, err = dial(s.addr, signer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return dir, c
}

// connect opens a PTY session running cmd (a shell when empty), with TERM=dumb
// so the renderer does not probe the terminal.
func connect(t *testing.T, c *gossh.Client, cmd string) (*gossh.Session, *syncBuf, func(string)) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	out := &syncBuf{}
	sess.Stdout, sess.Stderr = out, out
	if err := sess.RequestPty("dumb", 30, 100, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if cmd == "" {
		err = sess.Shell()
	} else {
		err = sess.Start(cmd)
	}
	if err != nil {
		t.Fatal(err)
	}
	return sess, out, func(keys string) { _, _ = in.Write([]byte(keys)) }
}

func TestCatalogNameOpensThatDashboard(t *testing.T) {
	_, c := catalogServer(t)
	for name, title := range map[string]string{"team-a": "Alpha", "team-b": "Beta"} {
		_, out, _ := connect(t, c, name)
		waitFor(t, out, "Panel of "+title)
	}
}

func TestCatalogUnknownNameListsNames(t *testing.T) {
	_, c := catalogServer(t)
	sess, out, _ := connect(t, c, "nope")
	err := sess.Wait()
	if ee, ok := err.(*gossh.ExitError); !ok || ee.ExitStatus() != 1 {
		t.Errorf("exit %v, want status 1", err)
	}
	if got := out.String(); !strings.Contains(got, `Unknown dashboard "nope"`) || !strings.Contains(got, "team-a, team-b") {
		t.Errorf("output %q", got)
	}

	// More than one argument, and a path, are no names either.
	for _, cmd := range []string{"team-a team-b", "../team-a"} {
		sess, out, _ := connect(t, c, cmd)
		if err := sess.Wait(); err == nil {
			t.Errorf("%q: session ended with status 0, output %q", cmd, out.String())
		}
	}
}

func TestCatalogPickerThenEnterOpensOne(t *testing.T) {
	_, c := catalogServer(t)

	_, out, send := connect(t, c, "")
	waitFor(t, out, "Choose a dashboard")
	waitFor(t, out, "Beta  (team-b)")
	send("\r")
	waitFor(t, out, "Panel of Alpha")

	_, out, send = connect(t, c, "")
	waitFor(t, out, "Choose a dashboard")
	send("j\r")
	waitFor(t, out, "Panel of Beta")

	// q leaves without opening anything.
	sess, out, send := connect(t, c, "")
	waitFor(t, out, "Choose a dashboard")
	send("q")
	if err := sess.Wait(); err != nil {
		t.Errorf("q: %v", err)
	}
	if strings.Contains(out.String(), "Panel of") {
		t.Errorf("q opened a dashboard: %q", out.String())
	}
}

func TestCatalogSingleDashboardOpensDirectly(t *testing.T) {
	dir, c := catalogServer(t)
	if err := os.Remove(filepath.Join(dir, "team-b.yaml")); err != nil {
		t.Fatal(err)
	}
	_, out, _ := connect(t, c, "")
	waitFor(t, out, "Panel of Alpha")
	if strings.Contains(out.String(), "Choose a dashboard") {
		t.Error("picker shown for a single dashboard")
	}
}

func TestCatalogNewFileAppearsOnNextConnection(t *testing.T) {
	dir, c := catalogServer(t)

	sess, out, _ := connect(t, c, "team-c")
	_ = sess.Wait()
	if !strings.Contains(out.String(), "Unknown dashboard") {
		t.Fatalf("output %q", out.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "team-c.yml"), []byte(catalogDash("Gamma")), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ = connect(t, c, "team-c")
	waitFor(t, out, "Panel of Gamma")

	// An invalid file added later is skipped; the others keep working.
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("bogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ = connect(t, c, "team-a")
	waitFor(t, out, "Panel of Alpha")
}
