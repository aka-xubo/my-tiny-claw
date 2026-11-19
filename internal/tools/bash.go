// internal/tools/bash.go
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

type BashTool struct {
	workDir  string
	manager  *TaskManager
	readOnly bool
}

// NewBashTool keeps the original one-shot behavior for embedded callers and
// read-only subagents. The main agent should use NewManagedBashTool.
func NewBashTool(workDir string) *BashTool {
	return &BashTool{workDir: workDir}
}

func NewManagedBashTool(workDir string, manager *TaskManager) *BashTool {
	return &BashTool{workDir: workDir, manager: manager}
}

func (t *BashTool) Name() string {
	return "bash"
}

func (t *BashTool) Definition() schema.ToolDefinition {
	description := "在当前工作区执行 bash 命令。短命令直接返回；需要持续运行的命令会返回 session_id，可使用 poll、list、stop 查看或管理后台会话。"
	if t.readOnly {
		description = "在只读隔离中同步执行 bash 搜索与读取命令，最多运行 30 秒。禁止文件写入和网络访问，也不能在 /tmp 等目录创建临时文件；mktemp、需要临时文件的部分 go 子命令会失败。不支持后台会话。"
	}
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: description,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "首次调用时执行的 bash 命令，例如 ls -la 或 npm run dev",
				},
				"action": map[string]interface{}{
					"type": "string", "enum": []string{"poll", "list", "stop"},
					"description": "管理已有会话：poll 读取输出，list 列出会话，stop 终止会话",
				},
				"session_id":       map[string]interface{}{"type": "string", "description": "由 bash 返回的会话 ID"},
				"yield_time_ms":    map[string]interface{}{"type": "integer", "description": "本次调用最多等待的毫秒数；默认 10000，最大 30000"},
				"cursor":           map[string]interface{}{"type": "integer", "description": "可选的日志读取游标"},
				"max_output_bytes": map[string]interface{}{"type": "integer", "description": "本次最多返回的日志字节数，最大 8000"},
			},
		},
	}
}

type bashArgs struct {
	Command        string `json:"command"`
	Action         string `json:"action"`
	SessionID      string `json:"session_id"`
	YieldTimeMS    *int   `json:"yield_time_ms"`
	Cursor         *int64 `json:"cursor"`
	MaxOutputBytes int    `json:"max_output_bytes"`
}

type bashResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func (t *BashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input bashArgs
	if err := json.Unmarshal(args, &input); err != nil {
		if t.manager != nil {
			return managedError(1, fmt.Sprintf("参数解析失败: %v", err)), nil
		}
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if t.manager == nil {
		if input.Action != "" || input.SessionID != "" {
			return "", fmt.Errorf("当前 bash 工具不支持会话管理")
		}
		if input.Command == "" {
			return "", fmt.Errorf("command 不能为空")
		}
		return t.executeSync(ctx, input.Command)
	}
	output, err := t.executeManaged(ctx, input)
	if err != nil {
		return managedError(1, err.Error()), nil
	}
	return output, nil
}

func (t *BashTool) executeManaged(ctx context.Context, input bashArgs) (string, error) {
	if input.Command != "" && input.SessionID != "" {
		return "", fmt.Errorf("command 与 session_id 不能同时提供")
	}
	if input.Action != "" && input.Command != "" {
		return "", fmt.Errorf("action 调用不能同时提供 command")
	}
	if input.Action == "" && input.Command == "" {
		return "", fmt.Errorf("首次调用必须提供 command，后续调用必须提供 action")
	}
	if input.Action != "" && input.Action != "poll" && input.Action != "list" && input.Action != "stop" {
		return "", fmt.Errorf("不支持的 action: %s", input.Action)
	}
	maxOutput := input.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = 8000
	}
	if maxOutput < 1 || maxOutput > 8000 {
		return "", fmt.Errorf("max_output_bytes 必须在 1 到 8000 之间")
	}
	wait, err := parseYieldTime(input.YieldTimeMS, input.Action == "poll")
	if err != nil {
		return "", err
	}
	ownerID := executionOwner(ctx)
	workDir := executionWorkDir(ctx, t.workDir)

	switch input.Action {
	case "list":
		if input.SessionID != "" {
			return "", fmt.Errorf("list 不需要 session_id")
		}
		items, err := t.manager.List(ownerID, workDir)
		if err != nil {
			return managedError(1, err.Error()), nil
		}
		return encodeBashResponse(bashResponse{Code: 0, Message: "会话列表", Data: items})
	case "stop":
		if input.SessionID == "" {
			return "", fmt.Errorf("stop 必须提供 session_id")
		}
		if err := t.manager.Stop(ctx, input.SessionID, ownerID, workDir); err != nil {
			return managedError(1, err.Error()), nil
		}
		snapshot, _ := t.manager.WaitUntil(context.Background(), ownerID, workDir, input.SessionID, 0, maxOutput)
		return encodeBashResponse(bashResponse{Code: 0, Message: "会话已停止", Data: snapshot})
	case "poll":
		if input.SessionID == "" {
			return "", fmt.Errorf("poll 必须提供 session_id")
		}
		snapshot, err := t.manager.Poll(ctx, ownerID, workDir, input.SessionID, wait, input.Cursor, maxOutput)
		if err != nil {
			return managedError(1, err.Error()), nil
		}
		return encodeBashResponse(bashResponse{Code: 0, Message: "会话输出", Data: snapshot})
	default:
		if input.Command == "" {
			return "", fmt.Errorf("command 不能为空")
		}
		sessionID, err := t.manager.Start(ctx, ownerID, workDir, input.Command)
		if err != nil {
			code := 1
			if errors.Is(err, ErrTaskStart) {
				code = 2
			}
			if err == ErrTaskLimit {
				code = 3
			}
			return managedError(code, err.Error()), nil
		}
		snapshot, err := t.manager.WaitUntil(ctx, ownerID, workDir, sessionID, wait, maxOutput)
		if err != nil {
			return managedError(1, err.Error()), nil
		}
		message := "命令执行完成"
		if snapshot.Status == TaskRunning {
			message = "命令仍在运行，已返回会话"
		}
		return encodeBashResponse(bashResponse{Code: 0, Message: message, Data: snapshot})
	}
}

func (t *BashTool) executeSync(ctx context.Context, command string) (string, error) {
	if t.readOnly {
		return t.executeReadOnly(ctx, command)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 在 macOS/Linux 下，我们通过将指令包裹在 `bash -c` 中执行，以支持环境变量、管道和逻辑与(&&)等复杂 Shell 语法。
	cmd := exec.CommandContext(timeoutCtx, "bash", "-c", command)

	// 【驾驭底线 2】：绑定执行的工作区目录
	// 确保命令默认在用户指定的 WorkDir 下执行，而不是引擎启动时的绝对路径。
	cmd.Dir = t.workDir

	// 执行并捕获 CombinedOutput (合并 stdout 和 stderr)
	out, err := cmd.CombinedOutput()
	outputStr := string(out)

	// 如果命令执行超时，返回警告信息让模型知晓
	if timeoutCtx.Err() == context.DeadlineExceeded {
		return outputStr + "\n[警告: 命令执行超时(30s)，已被系统强制终止。]", nil
	}

	// 【驾驭底线 3】：错误原样回传 (Self-Correction 自愈机制)
	// 当 bash 报错时（err != nil），我们绝对不能返回 Go 的 error 阻断程序！
	// 我们必须把 err 和 outputStr 拼接成字符串返回，利用大模型的自纠错能力自己分析报错！
	if err != nil {
		return fmt.Sprintf("执行报错: %v\n输出:\n%s", err, outputStr), nil
	}

	// 如果没有终端输出（比如仅仅执行了 mkdir），给模型一个明确的执行成功的反馈
	if outputStr == "" {
		return "命令执行成功，无终端输出。", nil
	}

	// 【驾驭底线 4】：长度截断保护 (防 OOM)
	const maxLen = 8000
	if len(outputStr) > maxLen {
		return fmt.Sprintf("%s\n\n...[终端输出过长，已截断至前 %d 字节]...", outputStr[:maxLen], maxLen), nil
	}

	return outputStr, nil
}

func parseYieldTime(value *int, poll bool) (time.Duration, error) {
	if value == nil {
		return 10 * time.Second, nil
	}
	if *value == 0 && poll {
		return 0, nil
	}
	if *value < 250 || *value > 30000 {
		return 0, fmt.Errorf("yield_time_ms 必须在 250 到 30000 之间，poll 可使用 0")
	}
	return time.Duration(*value) * time.Millisecond, nil
}

func encodeBashResponse(response bashResponse) (string, error) {
	encoded, err := json.Marshal(response)
	if err != nil {
		return "", fmt.Errorf("编码 bash 响应失败: %w", err)
	}
	return string(encoded), nil
}

func managedError(code int, message string) string {
	encoded, _ := encodeBashResponse(bashResponse{Code: code, Message: message})
	return encoded
}
