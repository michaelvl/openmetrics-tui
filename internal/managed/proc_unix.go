//go:build unix && !linux

package managed

import "syscall"

// sysProcAttr uses a process group only; there is no Pdeathsig outside Linux,
// so a hard kill of the TUI can leave children behind (best effort).
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
