package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizePathResolvesExistingSymlinkPrefix(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := NormalizePath(filepath.Join(link, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := NormalizePath(target)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedTarget, "new.txt")
	if got != want {
		t.Fatalf("NormalizePath() = %q, want %q", got, want)
	}
}

func TestGlobToRegex(t *testing.T) {
	got, err := GlobToRegex("/workspace", "**/*.env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `(.*/)?`) || !strings.HasPrefix(got, `^/workspace/`) {
		t.Fatalf("unexpected regex: %s", got)
	}
	if err := ValidateGlob("[abc]"); err == nil {
		t.Fatal("expected unsupported character class to be rejected")
	}
}

func TestCleanEnvironment(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"CODEX_API_KEY=secret",
		"my_private=keep-or-drop",
		"SAFE=value",
	}
	got := CleanEnvironment(env, []string{"MY_PRIVATE"}, []string{"CODEX_API_KEY"})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "CODEX_API_KEY=secret") {
		t.Fatal("keep should override the builtin exclusion")
	}
	if strings.Contains(joined, "my_private=keep-or-drop") {
		t.Fatal("configured exclusion should remove the variable")
	}
	if !strings.Contains(joined, "SAFE=value") {
		t.Fatal("ordinary variables should remain")
	}
}

func TestGenerateWorkspaceSBPL(t *testing.T) {
	profiles, err := BuiltinProfiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	text, err := GenerateSBPL(profiles[":workspace"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(deny default)",
		"(allow file-read*",
		"(allow file-read* file-write*",
		".git",
		"file-write-unlink",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("SBPL missing %q:\n%s", want, text)
		}
	}
}
