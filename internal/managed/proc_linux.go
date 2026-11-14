//go:build linux

package managed

import "syscall"

// sysProcAttr puts the child in its own process group and asks the kernel to
// kill it if we die. Pdeathsig is tied to the thread that started the child,
// which is good enough here: the Go runtime keeps that thread alive for as
// long as our goroutine runs on it, and the extra cost of a stray kill is a
// restart.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
