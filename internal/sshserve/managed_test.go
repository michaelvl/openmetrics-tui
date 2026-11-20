package sshserve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
	"github.com/michaelvl/openmetrics-tui/internal/dashtui"
	"github.com/michaelvl/openmetrics-tui/internal/managed"
)

// fakeManaged is a managed backend with one child that can be killed. Like
// the real one it is shared by every session.
type fakeManaged struct {
	mu       sync.Mutex
	up       bool
	why      string // why the child is down
	events   managed.Broadcaster
	restarts int
}

func (f *fakeManaged) Health() []managed.ChildStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := managed.ChildStatus{Name: "victoria-metrics", Running: f.up}
	if !f.up {
		st.Err = errors.New(f.why)
	}
	return []managed.ChildStatus{st}
}

func (f *fakeManaged) Subscribe() (<-chan managed.Event, func()) { return f.events.Subscribe() }
func (f *fakeManaged) Logs() []string                            { return []string{"vm: started"} }

func (f *fakeManaged) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up = true
	f.restarts++
	return nil
}

func (f *fakeManaged) kill(why string) {
	f.mu.Lock()
	f.up, f.why = false, why
	f.mu.Unlock()
	f.events.Send(managed.Event{Child: "victoria-metrics", Err: errors.New(why)})
}

// A child failure shows in every open session at once, and R in one session
// recovers all of them.
func TestManagedBackendSharedBetweenSessions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team-a.yaml"), []byte(catalogDash("Alpha")), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := dashboard.LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	be := &fakeManaged{up: true}
	signer := newSigner(t)
	base := dashtui.Options{Querier: emptyQuerier{}, NoSnapshot: true, Mode: "managed in-cluster", Backend: be}
	s := start(t, Config{AuthKeysPath: writeKeys(t, signer)}, CatalogModel(cat, base))
	c, err := dial(s.addr, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	_, out1, send1 := connect(t, c, "team-a")
	_, out2, _ := connect(t, c, "team-a")
	waitFor(t, out1, "Panel of Alpha")
	waitFor(t, out2, "Panel of Alpha")

	be.kill("first failure")
	for _, out := range []*syncBuf{out1, out2} {
		waitFor(t, out, "backend down")
		waitFor(t, out, "victoria-metrics exited: first failure")
	}

	send1("R")
	deadline := time.Now().Add(5 * time.Second)
	for {
		be.mu.Lock()
		up, restarts := be.up, be.restarts
		be.mu.Unlock()
		if up && restarts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart did not run once: up %v, restarts %d", up, restarts)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Both sessions are still subscribed after the restart: a second failure
	// reaches them both.
	be.kill("second failure")
	for _, out := range []*syncBuf{out1, out2} {
		waitFor(t, out, "victoria-metrics exited: second failure")
	}
}
