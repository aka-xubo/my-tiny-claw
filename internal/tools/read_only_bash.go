package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

func NewReadOnlyBashTool(workDir string) *BashTool {
	profiles, err := sandbox.BuiltinProfiles(workDir)
	if err != nil {
		return &BashTool{workDir: workDir, readOnly: true, sessionErr: err}
	}
	session, err := sandbox.NewSession(profiles[":read-only"], sandbox.ApprovalNever, sandbox.DenyApprover{}, []string{workDir})
	if err != nil {
		return &BashTool{workDir: workDir, readOnly: true, sessionErr: err}
	}
	return NewReadOnlyBashToolWithSandbox(workDir, session)
}

func NewReadOnlyBashToolWithSandbox(workDir string, session *sandbox.Session) *BashTool {
	return &BashTool{workDir: workDir, readOnly: true, session: session}
}

// Preserve one-shot tools' bounded output and process-group lifetime while
// letting sandbox.Session own command construction and permission enforcement.
func runSyncCommand(ctx context.Context, cmd *exec.Cmd, profileName string) (string, error) {
	configureProcess(cmd)
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, true)
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	output := newTaskLog(defaultTaskLogLimit)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("无法启动沙箱命令，已拒绝执行: %w", err)
	}
	err := cmd.Wait()
	signalProcessGroup(cmd, true)
	text, _, truncated := output.tail(8000)
	if truncated {
		text += "\n[终端输出过长，已保留最后 8000 字节]"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return text + "\n[警告: 命令执行超时(最长30s)，已被系统强制终止。]", nil
	}
	if err != nil {
		if strings.Contains(text, "Operation not permitted") {
			text += "\n[sandbox] 命令可能被沙箱拦截（权限档 " + profileName + "）。如确实需要，可用 sandbox_permissions 申请扩大权限后重试。"
		}
		return fmt.Sprintf("执行报错: %v\n输出:\n%s", err, text), nil
	}
	if text == "" {
		text = "命令执行成功，无终端输出。"
	}
	return text, nil
}

// The protocol middleware limits available operations. The OS profile in
// sandbox.Session enforces the actual filesystem and network restrictions.
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
		if args.SandboxPermissions != "" && args.SandboxPermissions != sandbox.UseDefault {
			return false, "子 Agent 不允许申请额外权限或完全提权"
		}
		return true, ""
	}
}
