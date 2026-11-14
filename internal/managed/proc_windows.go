//go:build windows

package managed

import (
	"os"
	"syscall"
)

// Windows is untested; children are simply killed.
func sysProcAttr() *syscall.SysProcAttr { return nil }

func signalGroup(p *os.Process, _ bool) { _ = p.Kill() }
