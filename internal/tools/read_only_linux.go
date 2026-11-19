//go:build linux

package tools

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

// A read-only mount still exposes pathname Unix sockets (e.g. Docker/SSH).
// Deny socket creation and connection as well as namespace/ptrace escapes.
// Bubblewrap applies this filter before exec; descendants inherit it.
func restrictReadOnlyLinux(cmd *exec.Cmd) (func(), error) {
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	default:
		return nil, fmt.Errorf("当前 Linux 架构不支持只读 bash 隔离，已拒绝执行")
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4}, // seccomp_data.arch
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},           // syscall number
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: 0x3fffffff}, // x32 bit
	}
	for _, syscallNumber := range []uint32{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT,
		unix.SYS_PTRACE, unix.SYS_SETNS, unix.SYS_UNSHARE,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_REGISTER, unix.SYS_IO_URING_ENTER,
	} {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: syscallNumber, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	filter = append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	var program bytes.Buffer
	if err := binary.Write(&program, binary.NativeEndian, filter); err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	_, err = writer.Write(program.Bytes())
	writer.Close()
	if err != nil {
		reader.Close()
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{reader}
	return func() { reader.Close() }, nil
}
