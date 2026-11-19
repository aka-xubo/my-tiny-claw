//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
	"time"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	// Give the process group a short grace period before using SIGKILL. The
	// Wait goroutine owns the final state transition.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
