package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

type ReadSkillTool struct {
	loader *cctx.SkillLoader
}

func NewReadSkillTool(loader *cctx.SkillLoader) *ReadSkillTool {
	return &ReadSkillTool{loader: loader}
}

func (t *ReadSkillTool) Name() string {
	return "read_skill"
}

func (t *ReadSkillTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "根据技能名称读取对应的完整 SKILL.md 正文。只能使用 System Prompt 技能目录中列出的名称。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "System Prompt 技能目录中的准确技能名称",
				},
			},
			"required": []string{"name"},
		},
	}
}

type readSkillArgs struct {
	Name string `json:"name"`
}

func (t *ReadSkillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input readSkillArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("解析 read_skill 参数失败: %w", err)
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return "", fmt.Errorf("read_skill 的 name 不能为空")
	}
	if t.loader == nil {
		return "", fmt.Errorf("技能加载器未初始化")
	}

	skill, err := t.loader.LoadBody(name)
	if err != nil {
		return "", fmt.Errorf("读取技能 %q 失败: %w", name, err)
	}

	return fmt.Sprintf("[SKILL_CONTENT_BEGIN name=%s]\n%s\n[SKILL_CONTENT_END]", skill.Name, skill.Body), nil
}
