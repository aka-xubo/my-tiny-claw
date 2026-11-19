//go:build !windows

package tools

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalProcessGroup(cmd *exec.Cmd, force bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	// Signal only groups created and retained by this manager, including exited
	// shells with surviving children. Empty PGIDs may theoretically be reused;
	// ESRCH is expected, and we never fall back to signaling an unrelated PID.
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("[TaskManager] signal group %d (%v): %v", cmd.Process.Pid, sig, err)
	}
}

func processGroupAlive(cmd *exec.Cmd) bool {
	return !errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
}

func processSignal(state *os.ProcessState) string {
	if state != nil {
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return status.Signal().String()
		}
	}
	return ""
}
