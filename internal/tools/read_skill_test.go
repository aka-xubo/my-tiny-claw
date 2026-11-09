package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
)

func TestReadSkillToolReturnsDelimitedBody(t *testing.T) {
	workDir := t.TempDir()
	skillPath := filepath.Join(workDir, ".claw", "skills", "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: demo\ndescription: 测试技能\n---\n正文内容"
	if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadSkillTool(cctx.NewSkillLoader(workDir))
	args, _ := json.Marshal(map[string]string{"name": "demo"})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "[SKILL_CONTENT_BEGIN name=demo]") || !strings.Contains(output, "正文内容") || !strings.Contains(output, "[SKILL_CONTENT_END]") {
		t.Fatalf("read_skill 返回内容缺少边界或正文: %s", output)
	}
}
