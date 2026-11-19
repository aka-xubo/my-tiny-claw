//go:build !linux

package tools

import (
	"fmt"
	"os/exec"
)

func restrictReadOnlyLinux(_ *exec.Cmd) (func(), error) {
	return nil, fmt.Errorf("当前系统不支持 Linux 只读 bash 隔离")
}
