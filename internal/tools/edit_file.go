// internal/tools/edit_file.go
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aka-xubo/my-tiny-claw/internal/fshelper"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

type EditFileTool struct {
	workDir    string
	session    *sandbox.Session
	sessionErr error
}

func NewEditFileTool(workDir string) *EditFileTool {
	session, err := defaultSandboxSession(workDir)
	return &EditFileTool{workDir: workDir, session: session, sessionErr: err}
}

func NewEditFileToolWithSandbox(workDir string, session *sandbox.Session) *EditFileTool {
	return &EditFileTool{workDir: workDir, session: session}
}

func (t *EditFileTool) Name() string {
	return "edit_file"
}

func (t *EditFileTool) Definition() schema.ToolDefinition {
	result := schema.ToolDefinition{
		Name:        t.Name(),
		Description: "对现有文件进行局部的字符串替换。这比重写整个文件更安全、更快速。请提供足够的 old_text 上下文以确保匹配的唯一性。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "要修改的文件路径",
				},
				"old_text": map[string]interface{}{
					"type":        "string",
					"description": "文件中原有的文本。必须包含足够的上下文（建议上下各多包含几行），以确保在文件中的唯一性。",
				},
				"new_text": map[string]interface{}{
					"type":        "string",
					"description": "要替换成的新文本",
				},
			},
			"required": []string{"path", "old_text", "new_text"},
		},
	}
	addSandboxProperties(result.InputSchema.(map[string]interface{})["properties"].(map[string]interface{}))
	return result
}

// internal/tools/edit_file.go

func (t *EditFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input editFileArgs
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
	call, err := input.sandboxToolArgs.call("edit_file", "", t.workDir, "edit", path)
	if err != nil {
		return "", err
	}
	profile, err := t.session.Authorize(ctx, call)
	if err != nil {
		return "", err
	}
	if _, err := runFileHelper(ctx, t.session, profile, t.workDir, fshelper.Request{Op: "edit", Path: path, OldText: input.OldText, NewText: input.NewText}); err != nil {
		return "", err
	}
	return fmt.Sprintf("✅ 成功修改文件: %s", input.Path), nil

}

type editFileArgs struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
	sandboxToolArgs
}
