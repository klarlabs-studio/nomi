//go:build windows

package executor

import "syscall"

// sysProcAttr requests a new process group so the child isn't killed by
// Ctrl+C sent to the daemon's console.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}
