// internal/tools/write_file.go
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aka-xubo/my-tiny-claw/internal/fshelper"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

type WriteFileTool struct {
	workDir    string // 工作区约束
	session    *sandbox.Session
	sessionErr error
}

func NewWriteFileTool(workDir string) *WriteFileTool {
	session, err := defaultSandboxSession(workDir)
	return &WriteFileTool{workDir: workDir, session: session, sessionErr: err}
}

func NewWriteFileToolWithSandbox(workDir string, session *sandbox.Session) *WriteFileTool {
	return &WriteFileTool{workDir: workDir, session: session}
}

func (t *WriteFileTool) Name() string {
	return "write_file"
}

func (t *WriteFileTool) Definition() schema.ToolDefinition {
	result := schema.ToolDefinition{
		Name:        t.Name(),
		Description: "创建或覆盖写入一个文件。如果目录不存在会自动创建。支持工作区相对路径和绝对路径；可写范围由权限档决定。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "要写入的文件路径，如 src/main.go",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "要写入的完整文件内容",
				},
			},
			"required": []string{"path", "content"},
		},
	}
	addSandboxProperties(result.InputSchema.(map[string]interface{})["properties"].(map[string]interface{}))
	return result
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	sandboxToolArgs
}

func (t *WriteFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input writeFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if t.sessionErr != nil {
		return "", t.sessionErr
	}
	if t.session == nil {
		return "", fmt.Errorf("沙箱会话未初始化，已拒绝执行")
	}

	path := resolveToolPath(t.workDir, input.Path)
	call, err := input.sandboxToolArgs.call("write_file", "", t.workDir, "write", path)
	if err != nil {
		return "", err
	}
	profile, err := t.session.Authorize(ctx, call)
	if err != nil {
		return "", err
	}
	if _, err := runFileHelper(ctx, t.session, profile, t.workDir, fshelper.Request{Op: "write", Path: path, Content: input.Content, CreateParents: true}); err != nil {
		return "", err
	}
	return fmt.Sprintf("成功将内容写入到文件: %s", input.Path), nil

}
