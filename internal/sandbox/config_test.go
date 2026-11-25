package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildConfigExtendsAndWorkspaceRoots(t *testing.T) {
	workspace := t.TempDir()
	raw := map[string]any{
		"default_permissions": "dev",
		"approval_policy":     "never",
		"permissions": map[string]any{
			"dev": map[string]any{
				"extends": " :workspace",
			},
		},
	}
	// Use the real TOML decoder shape through a temporary file in the loader
	// path only for the trust test below; this assertion focuses on defaults.
	profiles, err := BuiltinProfiles(workspace)
	if err != nil {
		t.Fatal(err)
	}
	raw["permissions"].(map[string]any)["dev"].(map[string]any)["extends"] = ":workspace"
	config, err := buildConfig(workspace, profiles, raw)
	if err != nil {
		t.Fatal(err)
	}
	if config.DefaultPermissions != "dev" || config.ApprovalPolicy != ApprovalNever {
		t.Fatalf("unexpected config: %+v", config)
	}
	if _, ok := config.Profiles["dev"]; !ok {
		t.Fatal("extended profile was not built")
	}
}

func TestProjectConfigRequiresExplicitTrust(t *testing.T) {
	workspace := t.TempDir()
	projectDir := filepath.Join(workspace, ".claw")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config.toml"), []byte("approval_policy = \"never\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	user := map[string]any{}
	if projectTrusted(user, workspace) {
		t.Fatal("unlisted project should not be trusted")
	}
	user["projects"] = map[string]any{workspace: map[string]any{"trust_level": "trusted"}}
	if !projectTrusted(user, workspace) {
		t.Fatal("explicitly trusted project was not recognized")
	}
}

func TestReadTOMLConfigShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := []byte(`[permissions.dev]
extends = ":workspace"
[permissions.dev.filesystem.":workspace_roots"]
"**/*.env" = "deny"
[permissions.dev.network]
mode = "proxy"
[permissions.dev.network.domains]
"*.example.com" = "allow"
[shell_environment]
exclude = ["PRIVATE_*"]
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	raw, err := readTOMLIfExists(path)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := BuiltinProfiles(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	config, err := buildConfig(filepath.Dir(path), profiles, raw)
	if err != nil {
		t.Fatal(err)
	}
	profile := config.Profiles["dev"]
	if profile == nil || profile.Network != NetworkProxy || len(profile.Domains) != 1 {
		t.Fatalf("unexpected profile: %+v", profile)
	}
	if len(config.EnvExclude) != 1 || config.EnvExclude[0] != "PRIVATE_*" {
		t.Fatalf("unexpected environment config: %+v", config.EnvExclude)
	}
}

func TestConfigTrustAndLayerMerge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	normalized, err := NormalizePath(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claw"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".claw"), 0700); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(home, ".claw", "config.toml")
	projectPath := filepath.Join(ws, ".claw", "config.toml")
	user := `default_permissions = "dev"
[permissions.dev]
extends = ":workspace"
[permissions.dev.network]
mode = "disabled"
[shell_environment]
exclude = ["USER_*"]
keep = ["KEEP_ME"]
`
	if err := os.WriteFile(userPath, []byte(user), 0600); err != nil {
		t.Fatal(err)
	}
	// An untrusted, even syntactically invalid file is completely ignored.
	if err := os.WriteFile(projectPath, []byte("invalid = ["), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(ws)
	if err != nil {
		t.Fatal(err)
	}
	if config.Profiles["dev"].Network != NetworkDisabled {
		t.Fatal("untrusted project applied")
	}
	if err := TrustProject(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(ws); err == nil {
		t.Fatal("trusted malformed file silently ignored")
	}
	project := `approval_policy = "never"
[permissions.dev.network]
mode = "proxy"
[permissions.dev.network.domains]
"EXAMPLE.COM." = "allow"
[shell_environment]
exclude = ["PROJECT_*"]
`
	if err := os.WriteFile(projectPath, []byte(project), 0600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(ws)
	if err != nil {
		t.Fatal(err)
	}
	if config.ApprovalPolicy != ApprovalNever || config.Profiles["dev"].Network != NetworkProxy || config.EnvExclude[0] != "PROJECT_*" || config.EnvKeep[0] != "KEEP_ME" {
		t.Fatalf("bad merge: %+v", config)
	}
	if config.Profiles["dev"].Domains["example.com"] != DomainAllow {
		t.Fatal("domain not normalized")
	}
	exists, trusted, err := ProjectConfigStatus(normalized)
	if err != nil || !exists || !trusted {
		t.Fatalf("trust status %v %v %v", exists, trusted, err)
	}
}
func TestConfigInvalidDefinitionsFailClosed(t *testing.T) {
	for _, test := range []struct{ name, text string }{
		{"cycle", `[permissions.a]
extends="b"
[permissions.b]
extends="a"`},
		{"builtin", `[permissions.":workspace"]
description="override"`},
		{"missing", `default_permissions="missing"`},
		{"type", `approval_policy=12`},
		{"glob", `[permissions.dev.filesystem.":workspace_roots"]
"**/[abc]"="deny"`},
		{"escape", `[permissions.dev.filesystem.":workspace_roots"]
"../outside"="write"`},
		{"domain", `[permissions.dev.network.domains]
"*.*.com"="allow"`},
		{"network", `[permissions.dev.network]
mode="invalid"`},
		{"env", `[shell_environment]
keep=[12]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.text), 0600); err != nil {
				t.Fatal(err)
			}
			raw, err := readTOMLIfExists(path)
			if err != nil {
				t.Fatal(err)
			}
			profiles, _ := BuiltinProfiles(filepath.Dir(path))
			if _, err = buildConfig(filepath.Dir(path), profiles, raw); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
func TestConfigGlobAndPlaceholderResolution(t *testing.T) {
	root := t.TempDir()
	p := &Profile{}
	for _, path := range []string{filepath.Join(root, "**", "*.env"), ":tmpdir", "~"} {
		if err := addFileRule(p, path, "deny"); err != nil {
			t.Fatal(err)
		}
	}
	if p.Files[0].Glob != "**/*.env" {
		t.Fatalf("glob=%q", p.Files[0].Glob)
	}
	if p.Files[1].Path == ":tmpdir" || p.Files[2].Path == "~" {
		t.Fatal("placeholder not expanded")
	}
}
