//go:build windows

package tools

import "os/exec"

// Windows Job Object support is intentionally deferred to a later phase. This
// keeps the first implementation buildable on Windows while preserving the
// process control abstraction used by TaskManager.
func configureProcess(cmd *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd, _ <-chan struct{}) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
