//go:build unix

package managed

import (
	"os"
	"syscall"
)

// signalGroup sends SIGTERM, or SIGKILL when kill is set, to the child's
// whole process group.
func signalGroup(p *os.Process, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-p.Pid, sig); err != nil {
		_ = p.Signal(sig)
	}
}
