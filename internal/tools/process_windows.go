//go:build windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// Job Objects are deferred; Windows currently stops only the direct child.
func configureProcess(cmd *exec.Cmd) {}

func signalProcessGroup(cmd *exec.Cmd, _ bool) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func processGroupAlive(cmd *exec.Cmd) bool {
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	status, err := syscall.WaitForSingleObject(handle, 0)
	return err == nil && status == syscall.WAIT_TIMEOUT
}

func processSignal(_ *os.ProcessState) string { return "" }
