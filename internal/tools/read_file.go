// internal/tools/read_file.go
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aka-xubo/my-tiny-claw/internal/fshelper"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

// ReadFileTool 实现了读取本地文件内容的工具
type ReadFileTool struct {
	// 相对路径以工作区为基准，访问范围由沙箱权限档控制。
	workDir    string
	session    *sandbox.Session
	sessionErr error
}

func NewReadFileTool(workDir string) *ReadFileTool {
	session, err := defaultSandboxSession(workDir)
	return &ReadFileTool{workDir: workDir, session: session, sessionErr: err}
}

func NewReadFileToolWithSandbox(workDir string, session *sandbox.Session) *ReadFileTool {
	return &ReadFileTool{workDir: workDir, session: session}
}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

// Definition 向大模型清晰地描述这个工具的用途和参数格式
func (t *ReadFileTool) Definition() schema.ToolDefinition {
	result := schema.ToolDefinition{
		Name:        t.Name(),
		Description: "读取权限档允许的文件，支持工作区相对路径和绝对路径。默认档允许宽泛读取，内容将进入模型上下文。",
		// 遵循 JSON Schema 规范定义参数
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "要读取的文件路径，如 cmd/claw/main.go",
				},
			},
			"required": []string{"path"},
		},
	}
	addSandboxProperties(result.InputSchema.(map[string]interface{})["properties"].(map[string]interface{}))
	return result
}

// readFileArgs 内部定义用于反序列化的结构体
type readFileArgs struct {
	Path string `json:"path"`
	sandboxToolArgs
}

func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// 1. 延迟解析：将大模型传过来的 JSON 参数解析为强类型结构体
	var input readFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		// 返回 error 会被 Registry 捕获并传给大模型，模型会知道自己 JSON 格式写错了
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if t.sessionErr != nil {
		return "", t.sessionErr
	}
	if t.session == nil {
		return "", fmt.Errorf("沙箱会话未初始化，已拒绝执行")
	}

	path := resolveToolPath(t.workDir, input.Path)
	call, err := input.sandboxToolArgs.call("read_file", "", t.workDir, "read", path)
	if err != nil {
		return "", err
	}
	profile, err := t.session.Authorize(ctx, call)
	if err != nil {
		return "", err
	}
	response, err := runFileHelper(ctx, t.session, profile, t.workDir, fshelper.Request{Op: "read", Path: path, MaxBytes: 262144})
	if err != nil {
		return "", err
	}
	content := response.Content
	const maxLen = 8000
	if len(content) > maxLen {
		return fmt.Sprintf("%s\n\n...[由于内容过长，已被系统截断至前 %d 字节]...", content[:maxLen], maxLen), nil
	}
	return content, nil

}
