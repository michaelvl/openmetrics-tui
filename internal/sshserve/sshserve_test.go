package sshserve

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// stub shows the last window size it was given and quits on q.
type stub struct{ w, h int }

func (stub) Init() tea.Cmd { return nil }

func (s stub) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.w, s.h = msg.Width, msg.Height
	case tea.KeyMsg:
		if msg.String() == "q" {
			return s, tea.Quit
		}
	}
	return s, nil
}

func (s stub) View() string { return fmt.Sprintf("stub %dx%d", s.w, s.h) }

func newStub(context.Context, ssh.Session) (tea.Model, []tea.ProgramOption) {
	return stub{}, nil
}

func newSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeKeys(t *testing.T, signers ...gossh.Signer) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("# test keys\n\n")
	for _, s := range signers {
		b.Write(gossh.MarshalAuthorizedKey(s.PublicKey()))
	}
	p := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type server struct {
	addr   string
	cancel context.CancelFunc
	done   chan struct{} // closed when Serve returns
	err    error
	finger string
}

// start runs Serve on a free loopback port and waits until it listens.
func start(t *testing.T, cfg Config, nm NewModel) *server {
	t.Helper()
	cfg.Listen = "127.0.0.1:0"
	if cfg.HostKeyPath == "" {
		cfg.HostKeyPath = filepath.Join(t.TempDir(), "host")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{cancel: cancel, done: make(chan struct{})}
	ready := make(chan struct{})
	cfg.OnListen = func(a net.Addr, f string) {
		s.addr, s.finger = a.String(), f
		close(ready)
	}
	go func() { s.err = Serve(ctx, cfg, nm); close(s.done) }()
	select {
	case <-ready:
	case <-s.done:
		t.Fatalf("Serve: %v", s.err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return s
}

func dial(addr string, signer gossh.Signer) (*gossh.Client, error) {
	return gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "u",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

// syncBuf is an output sink the test can poll while the session writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// shell opens a PTY session with a shell and collects its output.
func shell(t *testing.T, c *gossh.Client, pty bool) (*gossh.Session, *syncBuf) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	out := &syncBuf{}
	sess.Stdout, sess.Stderr = out, out
	if pty {
		if err := sess.RequestPty("xterm-256color", 30, 100, gossh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	return sess, out
}

func waitFor(t *testing.T, out *syncBuf, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q, got %q", want, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestListedKeyAcceptedUnlistedRejected(t *testing.T) {
	good, bad := newSigner(t), newSigner(t)
	s := start(t, Config{AuthKeysPath: writeKeys(t, good)}, newStub)

	c, err := dial(s.addr, good)
	if err != nil {
		t.Fatalf("listed key rejected: %v", err)
	}
	defer c.Close()
	_, out := shell(t, c, true)
	waitFor(t, out, "stub ")

	if c, err := dial(s.addr, bad); err == nil {
		_ = c.Close()
		t.Fatal("unlisted key accepted")
	}
}

func TestAuthorizedKeysReadOnEveryAttempt(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	path := writeKeys(t, a)
	s := start(t, Config{AuthKeysPath: path}, newStub)

	if c, err := dial(s.addr, b); err == nil {
		_ = c.Close()
		t.Fatal("key accepted before it was listed")
	}
	if err := os.WriteFile(path, append(gossh.MarshalAuthorizedKey(a.PublicKey()), gossh.MarshalAuthorizedKey(b.PublicKey())...), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := dial(s.addr, b)
	if err != nil {
		t.Fatalf("key rejected after it was listed: %v", err)
	}
	_ = c.Close()
}

func TestAllowAnyKey(t *testing.T) {
	s := start(t, Config{AllowAnyKey: true}, newStub)
	c, err := dial(s.addr, newSigner(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

func TestRefusesWithoutAuthConfig(t *testing.T) {
	cfg := Config{Listen: "127.0.0.1:0", HostKeyPath: filepath.Join(t.TempDir(), "host")}
	if err := Serve(context.Background(), cfg, newStub); err == nil {
		t.Error("no keys and no AllowAnyKey: want an error")
	}
	cfg.AuthKeysPath = filepath.Join(t.TempDir(), "missing")
	if err := Serve(context.Background(), cfg, newStub); err == nil {
		t.Error("missing authorized keys file: want an error")
	}
}

func TestHostKeyGeneratedAndStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host")
	s1 := start(t, Config{AllowAnyKey: true, HostKeyPath: path}, newStub)
	if !strings.HasPrefix(s1.finger, "SHA256:") {
		t.Errorf("fingerprint %q", s1.finger)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("host key mode %v, want 0600", fi.Mode().Perm())
	}
	s1.cancel()
	<-s1.done
	s2 := start(t, Config{AllowAnyKey: true, HostKeyPath: path}, newStub)
	if s2.finger != s1.finger {
		t.Errorf("fingerprint changed across restarts: %s then %s", s1.finger, s2.finger)
	}
}

func TestRejectsSessionWithoutPTY(t *testing.T) {
	s := start(t, Config{AllowAnyKey: true}, newStub)
	c, err := dial(s.addr, newSigner(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, out := shell(t, c, false)
	err = sess.Wait()
	if ee, ok := err.(*gossh.ExitError); !ok || ee.ExitStatus() != 1 {
		t.Errorf("Wait: %v, want exit status 1", err)
	}
	if !strings.Contains(out.String(), "Requires an active PTY") {
		t.Errorf("output %q", out.String())
	}
}

func TestWindowSizeReachesModel(t *testing.T) {
	s := start(t, Config{AllowAnyKey: true}, newStub)
	c, err := dial(s.addr, newSigner(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, out := shell(t, c, true)
	waitFor(t, out, "stub 100x30")
	if err := sess.WindowChange(25, 90); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "stub 90x25")
}

func TestSessionCap(t *testing.T) {
	s := start(t, Config{AllowAnyKey: true, MaxSessions: 1}, newStub)
	signer := newSigner(t)
	c1, err := dial(s.addr, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	_, out1 := shell(t, c1, true)
	waitFor(t, out1, "stub ")

	c2, err := dial(s.addr, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	sess2, out2 := shell(t, c2, true)
	_ = sess2.Wait()
	if !strings.Contains(out2.String(), "Too many sessions") {
		t.Errorf("second session output %q", out2.String())
	}

	// Disconnecting the first client frees its slot; the release is asynchronous.
	_ = c1.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess3, out3 := shell(t, c2, true)
		time.Sleep(200 * time.Millisecond)
		if strings.Contains(out3.String(), "stub ") {
			break
		}
		_ = sess3.Close()
		if time.Now().After(deadline) {
			t.Fatalf("slot never freed, last output %q", out3.String())
		}
	}
}

func TestCancelEndsSessionsAndListener(t *testing.T) {
	s := start(t, Config{AllowAnyKey: true}, newStub)
	c, err := dial(s.addr, newSigner(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, out := shell(t, c, true)
	waitFor(t, out, "stub ")

	s.cancel()
	// The session ends on its own; the connection is the client's to close, as
	// ssh does when its last session ends.
	waited := make(chan struct{})
	go func() { _ = sess.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("session still open after shutdown")
	}
	_ = c.Close()
	select {
	case <-s.done:
		if s.err != nil {
			t.Errorf("Serve: %v", s.err)
		}
	case <-time.After(shutdownTimeout - time.Second):
		t.Fatal("Serve did not return before the shutdown timeout")
	}
	if c2, err := net.DialTimeout("tcp", s.addr, time.Second); err == nil {
		_ = c2.Close()
		t.Error("listener still accepting")
	}
}
