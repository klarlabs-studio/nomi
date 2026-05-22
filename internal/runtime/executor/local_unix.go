//go:build !windows

package executor

import "syscall"

// sysProcAttr starts the child in a new session/process group so signals
// sent to the daemon don't propagate and the child's tty control doesn't
// leak into command output.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
