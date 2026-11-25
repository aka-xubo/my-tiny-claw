//go:build darwin

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareWorkspaceEnforcesFilesystemRules(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		if os.Getenv("CLAW_REQUIRE_SEATBELT") == "1" {
			t.Fatal(err)
		}
		t.Skip("sandbox-exec 不可用")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	protected := filepath.Join(workspace, ".git")
	if err := os.Mkdir(protected, 0755); err != nil {
		t.Fatal(err)
	}
	profile := &Profile{
		Name: ":test",
		Files: []FileRule{
			{Path: "/", Access: AccessRead},
			{Path: workspace, Access: AccessWrite},
			{Path: protected, Access: AccessRead},
		},
	}

	allowed := filepath.Join(workspace, "allowed.txt")
	cmd, cleanup, err := Prepare(context.Background(), SpawnRequest{
		Argv:    []string{"/bin/bash", "-c", "echo allowed > " + shellQuote(allowed)},
		Dir:     workspace,
		Env:     os.Environ(),
		Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if output, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "sandbox_apply: Operation not permitted") && os.Getenv("CLAW_REQUIRE_SEATBELT") != "1" {
			t.Skipf("当前运行环境禁止 sandbox-exec: %s", output)
		}
		t.Fatalf("workspace write failed: %v (%s)", err, output)
	}

	denied := filepath.Join(outside, "denied.txt")
	cmd, cleanup, err = Prepare(context.Background(), SpawnRequest{
		Argv:    []string{"/bin/bash", "-c", "echo denied > " + shellQuote(denied)},
		Dir:     workspace,
		Env:     os.Environ(),
		Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "Operation not permitted") {
		t.Fatalf("outside write was not denied: err=%v output=%s", err, output)
	}
	if _, err := os.Stat(denied); !os.IsNotExist(err) {
		t.Fatalf("denied file exists, stat error: %v", err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
