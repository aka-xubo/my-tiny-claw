package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillLoaderProgressiveLoadingAndCacheInvalidation(t *testing.T) {
	workDir := t.TempDir()
	skillPath := filepath.Join(workDir, ".claw", "skills", "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0755); err != nil {
		t.Fatal(err)
	}
	firstBody := "# Demo\n\n这是技能正文。"
	writeSkillFile(t, skillPath, "demo", "用于测试渐进式加载", firstBody)

	loader := NewSkillLoader(workDir)
	descriptors := loader.ListDescriptors()
	if len(descriptors) != 1 {
		t.Fatalf("技能目录数量 = %d, want 1", len(descriptors))
	}
	if descriptors[0].Name != "demo" || descriptors[0].Description != "用于测试渐进式加载" {
		t.Fatalf("技能元数据不正确: %+v", descriptors[0])
	}
	if strings.Contains(descriptors[0].Description, firstBody) {
		t.Fatal("技能正文不应出现在元数据中")
	}

	skill, err := loader.LoadBody("demo")
	if err != nil {
		t.Fatal(err)
	}
	if skill.Body != firstBody {
		t.Fatalf("技能正文 = %q, want %q", skill.Body, firstBody)
	}

	secondBody := "# Demo\n\n更新后的技能正文，缓存必须失效。"
	writeSkillFile(t, skillPath, "demo", "用于测试渐进式加载", secondBody)
	updated, err := loader.LoadBody("demo")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Body != secondBody {
		t.Fatalf("缓存未失效，正文 = %q", updated.Body)
	}
}

func TestSkillLoaderRejectsInvalidNamesAndOversizedBody(t *testing.T) {
	loader := NewSkillLoader(t.TempDir())
	if _, err := loader.LoadBody("../secret"); err == nil {
		t.Fatal("目录穿越技能名称应该被拒绝")
	}

	workDir := t.TempDir()
	skillPath := filepath.Join(workDir, ".claw", "skills", "large", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0755); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, skillPath, "large", "大正文", strings.Repeat("x", MaxSkillBodyBytes+1))

	loader = NewSkillLoader(workDir)
	if _, err := loader.LoadBody("large"); err == nil {
		t.Fatal("超大技能正文应该被拒绝")
	}
}

func TestSkillLoaderRejectsSkillSymlinkOutsideDirectory(t *testing.T) {
	workDir := t.TempDir()
	externalDir := t.TempDir()
	externalSkill := filepath.Join(externalDir, "SKILL.md")
	writeSkillFile(t, externalSkill, "external", "外部技能", "不应被加载")

	linkedSkill := filepath.Join(workDir, ".claw", "skills", "external", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(linkedSkill), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalSkill, linkedSkill); err != nil {
		t.Skipf("当前环境不支持符号链接: %v", err)
	}

	loader := NewSkillLoader(workDir)
	if _, err := loader.LoadBody("external"); err == nil {
		t.Fatal("指向技能目录外部的符号链接应该被拒绝")
	}
}

func TestParseSkillDocumentAllowsEmptyBodyAndBOM(t *testing.T) {
	descriptor := SkillDescriptor{Name: "demo", Description: "测试"}
	skill, err := parseSkillDocument("\ufeff---\r\nname: demo\r\ndescription: 测试\r\n---", descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if skill.Body != "" {
		t.Fatalf("空正文 = %q, want empty", skill.Body)
	}
}

func writeSkillFile(t *testing.T, path, name, description, body string) {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
