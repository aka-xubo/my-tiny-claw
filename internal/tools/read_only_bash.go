package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

func NewReadOnlyBashTool(workDir string) *BashTool {
	return &BashTool{workDir: workDir, readOnly: true}
}

// The middleware limits the protocol. The tool itself enforces OS isolation,
// including when called directly, rather than attempting to classify shell text.
func NewReadOnlyBashMiddleware() MiddlewareFunc {
	return func(_ context.Context, call schema.ToolCall) (bool, string) {
		if call.Name != "bash" {
			return true, ""
		}
		var args bashArgs
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return false, fmt.Sprintf("bash 参数解析失败: %v", err)
		}
		if args.Action != "" || args.SessionID != "" {
			return false, "子 Agent 不允许管理后台会话"
		}
		if args.Command == "" {
			return false, "bash 命令不能为空"
		}
		return true, ""
	}
}

// Descendants inherit the sandbox: flags, scripts, shell quoting and command
// substitutions cannot escape the filesystem restriction. No writable mounts
// or network are provided. Unsupported hosts fail closed.
func readOnlyCommand(ctx context.Context, command string) (*exec.Cmd, func(), error) {
	switch runtime.GOOS {
	case "darwin":
		const profile = `(version 1)
(deny default)
(allow file-read*)
(allow file-write-data (literal "/dev/null"))
(allow process-exec)
(allow process-fork)
(allow sysctl-read)`
		return exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile,
			"/bin/bash", "--noprofile", "--norc", "-c", command), func() {}, nil
	case "linux":
		// Use the system-installed bubblewrap, never a workspace/PATH executable.
		cmd := exec.CommandContext(ctx, "/usr/bin/bwrap", "--unshare-all",
			"--die-with-parent", "--new-session", "--cap-drop", "ALL",
			"--ro-bind", "/", "/", "--proc", "/proc", "--remount-ro", "/proc",
			"--dev", "/dev", "--seccomp", "3",
			"/bin/bash", "--noprofile", "--norc", "-c", command)
		cleanup, err := restrictReadOnlyLinux(cmd)
		return cmd, cleanup, err
	default:
		return nil, nil, fmt.Errorf("当前系统不支持只读 bash 隔离，已拒绝执行")
	}
}

func (t *BashTool) executeReadOnly(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd, cleanup, err := readOnlyCommand(ctx, command)
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmd.Dir = t.workDir
	configureProcess(cmd)
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, true)
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	// Bound memory even if a sandboxed command prints indefinitely.
	output := newTaskLog(defaultTaskLogLimit)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("无法启动只读 bash 隔离，已拒绝执行: %w", err)
	}
	err = cmd.Wait()
	// The one-shot tool never leaves background descendants behind.
	signalProcessGroup(cmd, true)
	text, _, truncated := output.tail(8000)
	if truncated {
		text += "\n[终端输出过长，已保留最后 8000 字节]"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return text + "\n[警告: 命令执行超时(最长30s)，已被系统强制终止。]", nil
	}
	// Match the one-shot BashTool contract: command failures are ordinary
	// results so the model can inspect diagnostics and correct its command.
	if err != nil {
		return fmt.Sprintf("执行报错: %v\n输出:\n%s", err, text), nil
	}
	if text == "" {
		text = "命令执行成功，无终端输出。"
	}
	return text, nil
}
