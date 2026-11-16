package headless

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeSource struct{ vars map[string]string }

func (f *fakeSource) Screenshot(context.Context) (string, error) { return "screen\n", nil }

func (f *fakeSource) SetVariable(_ context.Context, name, value string) error {
	switch {
	case name == "nope":
		return fmt.Errorf("%w: no variable %q", ErrNotFound, name)
	case value == "bad":
		return fmt.Errorf("%w: bad", ErrInvalid)
	}
	f.vars[name] = value
	return nil
}

type screenOnly struct{}

func (screenOnly) Screenshot(context.Context) (string, error) { return "x", nil }

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestScreenshot(t *testing.T) {
	rec := do(Handler(&fakeSource{}), "GET", "/screenshot", "")
	if rec.Code != 200 || rec.Body.String() != "screen\n" {
		t.Errorf("got %d %q", rec.Code, rec.Body)
	}
}

func TestSetVariable(t *testing.T) {
	src := &fakeSource{vars: map[string]string{}}
	h := Handler(src)
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"/variables/ns", "prod\n", 204},
		{"/variables/nope", "x", 404},
		{"/variables/ns", "bad", 400},
	} {
		if rec := do(h, "POST", tc.path, tc.body); rec.Code != tc.code {
			t.Errorf("POST %s %q = %d, want %d", tc.path, tc.body, rec.Code, tc.code)
		}
	}
	if src.vars["ns"] != "prod" {
		t.Errorf("value = %q, want the body without its newline", src.vars["ns"])
	}
	if rec := do(h, "GET", "/variables/ns", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /variables/ns = %d", rec.Code)
	}
}

func TestNoVariablesRouteWithoutSetter(t *testing.T) {
	if rec := do(Handler(screenOnly{}), "POST", "/variables/ns", "x"); rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestAskTimesOutWhenProgramGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Ask(ctx, func(tea.Msg) {}, func(r chan<- string) tea.Msg { return nil })
	if err == nil {
		t.Error("want an error")
	}
}

func TestServeLoopbackOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := Serve(ctx, 0, Handler(screenOnly{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("addr = %q", addr)
	}
	resp, err := http.Get("http://" + addr + "/screenshot")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
