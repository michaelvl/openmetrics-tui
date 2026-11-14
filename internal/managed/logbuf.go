package managed

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// LogBuffer is a bounded ring of log lines shared by all children. Child
// output goes here and never to the terminal; an optional writer also
// receives every line (the --log-file).
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
	out   io.Writer
}

// NewLogBuffer keeps at most n lines. out may be nil.
func NewLogBuffer(n int, out io.Writer) *LogBuffer {
	return &LogBuffer{lines: make([]string, n), out: out}
}

// Add appends one line.
func (b *LogBuffer) Add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines[b.next] = line
	b.next++
	if b.next == len(b.lines) {
		b.next = 0
		b.full = true
	}
	if b.out != nil {
		_, _ = io.WriteString(b.out, line+"\n")
	}
}

// Lines returns the buffered lines, oldest first.
func (b *LogBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.full {
		return append([]string(nil), b.lines[:b.next]...)
	}
	out := make([]string, 0, len(b.lines))
	out = append(out, b.lines[b.next:]...)
	return append(out, b.lines[:b.next]...)
}

// Tail returns the last n lines.
func (b *LogBuffer) Tail(n int) []string {
	l := b.Lines()
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return l
}

// lineWriter splits a byte stream into lines and hands each to sink.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	sink func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.sink(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 64<<10 { // a line without newline must not grow without bound
		w.sink(string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.sink(string(w.buf))
		w.buf = nil
	}
}
