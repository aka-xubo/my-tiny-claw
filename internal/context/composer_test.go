package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptComposerInjectsMetadataWithoutSkillBody(t *testing.T) {
	workDir := t.TempDir()
	skillPath := filepath.Join(workDir, ".claw", "skills", "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0755); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, skillPath, "demo", "任务匹配时使用", "这是不应在启动时注入的正文")

	prompt := NewPromptComposer(workDir, false).Build().Content
	if !strings.Contains(prompt, "demo") || !strings.Contains(prompt, "任务匹配时使用") {
		t.Fatalf("Prompt 缺少技能元数据: %s", prompt)
	}
	if strings.Contains(prompt, "这是不应在启动时注入的正文") {
		t.Fatal("Prompt 不应包含技能正文")
	}
}
