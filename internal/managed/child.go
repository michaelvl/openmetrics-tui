package managed

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// child is one supervised process.
type child struct {
	name string
	cmd  *exec.Cmd
	done chan struct{} // closed once the process has exited and its output is drained
	err  error         // exit error, valid after done is closed
	log  *LogBuffer

	mu       sync.Mutex
	stopping bool
}

// startChild runs bin in its own process group with stdin closed and output
// captured into log (every line prefixed with the child name). onLine, if not
// nil, sees each raw output line. onExit runs once after the child has exited
// unless it was stopped on purpose.
func startChild(name, bin string, args, env []string, log *LogBuffer, onLine func(string), onExit func(error)) (*child, error) {
	c := &child{name: name, done: make(chan struct{}), log: log}
	sink := func(line string) {
		log.Add("[" + name + "] " + line)
		if onLine != nil {
			onLine(line)
		}
	}
	stdout := &lineWriter{sink: sink}
	stderr := &lineWriter{sink: sink}
	c.cmd = exec.Command(bin, args...)
	c.cmd.Stdin = nil // /dev/null: an auth plugin cannot hang on a prompt
	c.cmd.Stdout = stdout
	c.cmd.Stderr = stderr
	c.cmd.SysProcAttr = sysProcAttr()
	c.cmd.WaitDelay = 2 * time.Second // do not wait for stragglers holding the pipes
	if env != nil {
		c.cmd.Env = env
	}
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		err := c.cmd.Wait()
		stdout.flush()
		stderr.flush()
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil
		}
		c.err = err
		close(c.done)
		c.mu.Lock()
		quiet := c.stopping
		c.mu.Unlock()
		if !quiet && onExit != nil {
			if err == nil {
				err = errors.New("exited")
			}
			onExit(err)
		}
	}()
	return c, nil
}

// exited reports whether the process has exited.
func (c *child) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// stop terminates the process group: SIGTERM, wait up to grace, then SIGKILL.
func (c *child) stop(grace time.Duration) {
	c.mu.Lock()
	c.stopping = true
	c.mu.Unlock()
	if c.exited() {
		return
	}
	signalGroup(c.cmd.Process, false)
	select {
	case <-c.done:
		return
	case <-time.After(grace):
	}
	c.log.Add(fmt.Sprintf("[%s] still running after %s, sending SIGKILL", c.name, grace))
	signalGroup(c.cmd.Process, true)
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
}

// waitReady polls probe until it succeeds. It fails early if the child
// exits, and when ctx ends or timeout elapses.
func (c *child) waitReady(ctx context.Context, timeout time.Duration, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	var last error
	for {
		if c.exited() {
			return fmt.Errorf("%s exited before it was ready: %w", c.name, exitErr(c.err))
		}
		err := probe(ctx)
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%s not ready after %s: %w", c.name, timeout, last)
			}
			return ctx.Err()
		case <-c.done:
		case <-t.C:
		}
	}
}

func exitErr(err error) error {
	if err == nil {
		return errors.New("exit status 0")
	}
	return err
}

// StartError is a startup failure: the step that failed and the tail of the
// output that explains it. Nothing is left running when it is returned.
type StartError struct {
	Step string
	Err  error
	Tail []string
}

func (e *StartError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %v", e.Step, e.Err)
	if len(e.Tail) > 0 {
		b.WriteString("\n  output:")
		for _, l := range e.Tail {
			b.WriteString("\n    " + l)
		}
	}
	return b.String()
}

func (e *StartError) Unwrap() error { return e.Err }
