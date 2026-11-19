// Package sshserve serves one Bubble Tea model per SSH connection, with
// public-key authentication, a session cap and graceful shutdown.
package sshserve

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	"github.com/charmbracelet/wish/bubbletea"
	"github.com/charmbracelet/wish/logging"
	gossh "golang.org/x/crypto/ssh"
)

// shutdownTimeout is how long open sessions get to end once ctx is cancelled.
const shutdownTimeout = 5 * time.Second

// Config is the server setup.
type Config struct {
	// Listen is the address to listen on, for example ":2222".
	Listen string
	// HostKeyPath is the host key file; an ed25519 key is generated there when
	// the file is missing.
	HostKeyPath string
	// AuthKeysPath is an OpenSSH authorized_keys file, read on every attempt so
	// changes apply without a restart.
	AuthKeysPath string
	// AllowAnyKey accepts every public key and ignores AuthKeysPath.
	AllowAnyKey bool
	// MaxSessions caps concurrent sessions; values below 1 mean no cap.
	MaxSessions int
	// OnListen, when set, is called once the listener is up, with its address
	// and the host key fingerprint.
	OnListen func(addr net.Addr, fingerprint string)
}

// NewModel builds the model and program options for one connection. ctx ends
// when the connection closes or the server shuts down.
type NewModel func(ctx context.Context, sess ssh.Session) (tea.Model, []tea.ProgramOption)

// Serve accepts connections until ctx is cancelled, then shuts down gracefully.
func Serve(ctx context.Context, cfg Config, newModel NewModel) error {
	if !cfg.AllowAnyKey && cfg.AuthKeysPath == "" {
		return errors.New("no authorized keys file and public keys are not allowed from anyone")
	}
	if !cfg.AllowAnyKey {
		if _, err := authorizedKeys(cfg.AuthKeysPath); err != nil {
			return err
		}
	}
	handler := func(sess ssh.Session) (tea.Model, []tea.ProgramOption) {
		// Cancel the model's requests and program on shutdown too, so sessions
		// end promptly instead of waiting out the shutdown timeout.
		sctx, cancel := context.WithCancel(sess.Context())
		stop := context.AfterFunc(ctx, cancel)
		go func() {
			<-sctx.Done()
			stop()
		}()
		m, opts := newModel(sctx, sess)
		if m == nil {
			cancel()
			return nil, nil
		}
		return m, append(opts, tea.WithContext(sctx))
	}
	srv, err := wish.NewServer(
		wish.WithAddress(cfg.Listen),
		wish.WithHostKeyPath(cfg.HostKeyPath),
		wish.WithPublicKeyAuth(func(_ ssh.Context, key ssh.PublicKey) bool {
			return cfg.AllowAnyKey || authorized(cfg.AuthKeysPath, key)
		}),
		// The last middleware is the outermost.
		wish.WithMiddleware(
			bubbletea.Middleware(handler),
			activeterm.Middleware(),
			limit(cfg.MaxSessions),
			logging.Middleware(),
		),
	)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	if cfg.OnListen != nil {
		cfg.OnListen(ln.Addr(), gossh.FingerprintSHA256(srv.HostSigners[0].PublicKey()))
	}

	// Also stops the goroutine below when Serve fails on its own.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sctx, scancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer scancel()
		if srv.Shutdown(sctx) != nil {
			_ = srv.Close()
		}
	}()
	err = srv.Serve(ln)
	if errors.Is(err, ssh.ErrServerClosed) {
		err = nil
	}
	cancel()
	<-done
	return err
}

// limit rejects sessions beyond max, with a one-line message.
func limit(max int) wish.Middleware {
	if max < 1 {
		return func(next ssh.Handler) ssh.Handler { return next }
	}
	slots := make(chan struct{}, max)
	return func(next ssh.Handler) ssh.Handler {
		return func(sess ssh.Session) {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				next(sess)
			default:
				wish.Println(sess, "Too many sessions, try again later")
				_ = sess.Exit(1)
			}
		}
	}
}

// authorized reports whether key is listed in the authorized_keys file at
// path. A missing or unreadable file authorizes nobody.
func authorized(path string, key ssh.PublicKey) bool {
	keys, err := authorizedKeys(path)
	if err != nil {
		log.Printf("authorized keys: %v", err)
		return false
	}
	for _, k := range keys {
		if ssh.KeysEqual(key, k) {
			return true
		}
	}
	return false
}

// authorizedKeys parses an OpenSSH authorized_keys file. Blank lines, comments
// and lines that do not parse are skipped.
func authorizedKeys(path string) ([]ssh.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []ssh.PublicKey
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20) // keys with long options
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		k, _, _, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			log.Printf("authorized keys: %s:%d: %v", path, n, err)
			continue
		}
		keys = append(keys, k)
	}
	return keys, sc.Err()
}
