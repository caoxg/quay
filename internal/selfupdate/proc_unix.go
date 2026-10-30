//go:build !windows

package selfupdate

import (
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func running(pid int) bool { return syscall.Kill(pid, 0) == nil }
