// Package headless serves a running TUI over HTTP, so a script or an agent can
// read the current screen as text (and, for omtui-dash, set variables) without
// a terminal. The server only talks to the program through Bubble Tea
// messages, so the model stays single-threaded.
package headless

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Errors a VariableSetter returns to pick the HTTP status; wrap them with
// fmt.Errorf("%w: ...") to add detail.
var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid value")
)

// requestTimeout bounds how long a request waits for the program to answer,
// for instance when it has already quit.
const requestTimeout = 5 * time.Second

// maxBody caps a variable value.
const maxBody = 4096

// Source is the TUI as the server sees it.
type Source interface {
	// Screenshot returns the current screen as plain text.
	Screenshot(ctx context.Context) (string, error)
}

// VariableSetter is implemented by sources that have variables to set.
type VariableSetter interface {
	SetVariable(ctx context.Context, name, value string) error
}

// Ask sends the message mk builds to the program and waits for the reply. The
// reply channel is buffered, so the program never blocks on a caller that has
// given up.
func Ask[T any](ctx context.Context, send func(tea.Msg), mk func(reply chan<- T) tea.Msg) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	reply := make(chan T, 1)
	send(mk(reply))
	select {
	case v := <-reply:
		return v, nil
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("program did not answer: %w", ctx.Err())
	}
}

// Handler serves GET /screenshot and, when src is also a VariableSetter,
// POST /variables/<name>, whose body is the new value.
func Handler(src Source) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /screenshot", func(w http.ResponseWriter, r *http.Request) {
		s, err := src.Screenshot(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, s)
	})
	if vs, ok := src.(VariableSetter); ok {
		mux.HandleFunc("POST /variables/{name}", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
				return
			}
			// echo-style clients end the value with a newline nobody meant.
			value := strings.TrimRight(string(body), "\r\n")
			switch err := vs.SetVariable(r.Context(), r.PathValue("name"), value); {
			case err == nil:
				w.WriteHeader(http.StatusNoContent)
			case errors.Is(err, ErrNotFound):
				http.Error(w, err.Error(), http.StatusNotFound)
			case errors.Is(err, ErrInvalid):
				http.Error(w, err.Error(), http.StatusBadRequest)
			default:
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
			}
		})
	}
	return mux
}

// Serve listens on the loopback interface at port and serves h until ctx is
// done. It returns once the port is bound, with the address in use. Loopback
// only: the endpoints show cluster data and change what the dashboard queries,
// and have no authentication.
func Serve(ctx context.Context, port int, h http.Handler) (string, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	return ln.Addr().String(), nil
}
