package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Access describes the filesystem operations permitted by a rule.
type Access int

const (
	AccessDeny Access = iota
	AccessRead
	AccessWrite
)

type NetworkMode int

const (
	NetworkDisabled NetworkMode = iota
	NetworkProxy
	NetworkFull
)

type DomainRule string

const (
	DomainAllow DomainRule = "allow"
	DomainDeny  DomainRule = "deny"
)

type FileRule struct {
	Path     string
	Glob     string
	Access   Access
	Approved bool // User-approved additions take precedence over configured rules.
}

type Profile struct {
	Name           string
	Description    string
	FullAccess     bool
	Files          []FileRule
	Network        NetworkMode
	Domains        map[string]DomainRule
	ProtectedRoots []string
}

func BuiltinProfiles(workspace string) (map[string]*Profile, error) {
	workspace, err := NormalizePath(workspace)
	if err != nil {
		return nil, fmt.Errorf("规范化工作区失败: %w", err)
	}
	tmpDir, err := NormalizePath(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("规范化临时目录失败: %w", err)
	}
	privateTmp, err := NormalizePath("/private/tmp")
	if err != nil {
		return nil, fmt.Errorf("规范化 /private/tmp 失败: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("解析用户主目录失败: %w", err)
	}
	oauthPath, err := NormalizePath(filepath.Join(home, ".claw", "mcp-oauth.json"))
	if err != nil {
		return nil, fmt.Errorf("规范化 OAuth 文件失败: %w", err)
	}

	readOnly := &Profile{
		Name:        ":read-only",
		Description: "只读：全盘可读，不可写，无网络",
		Network:     NetworkDisabled,
		Files: []FileRule{
			{Path: "/", Access: AccessRead},
			{Path: oauthPath, Access: AccessDeny},
		},
	}
	workspaceProfile := &Profile{
		ProtectedRoots: []string{workspace, tmpDir, privateTmp},
		Name:           ":workspace",
		Description:    "工作区可写：全盘可读，工作区和临时目录可写，受保护目录只读，无网络",
		Network:        NetworkDisabled,
		Files: []FileRule{
			{Path: "/", Access: AccessRead},
			{Path: tmpDir, Access: AccessWrite},
			{Path: privateTmp, Access: AccessWrite},
			{Path: workspace, Access: AccessWrite},
			{Path: filepath.Join(workspace, ".git"), Access: AccessRead},
			{Path: filepath.Join(workspace, ".claw"), Access: AccessRead},
			{Path: filepath.Join(workspace, ".agents"), Access: AccessRead},
			{Path: filepath.Join(workspace, ".aws"), Access: AccessRead},
			{Path: oauthPath, Access: AccessDeny},
		},
	}
	full := &Profile{
		Name:        ":danger-full-access",
		Description: "无沙箱：等同于当前用户的完整权限",
		FullAccess:  true,
		Network:     NetworkFull,
	}
	return map[string]*Profile{
		readOnly.Name:         readOnly,
		workspaceProfile.Name: workspaceProfile,
		full.Name:             full,
	}, nil
}

// NormalizePath expands the home directory and resolves the longest existing
// symlink prefix. This makes /tmp and /private/tmp compare as the same path on
// macOS while still allowing rules for paths that do not exist yet.
func NormalizePath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("路径不能为空")
	}
	if strings.ContainsAny(raw, "\x00\n\r") {
		return "", errors.New("路径不能包含 NUL 或换行")
	}
	path := raw
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("解析用户主目录失败: %w", err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = abs
	}
	path = filepath.Clean(path)

	for candidate := path; ; candidate = filepath.Dir(candidate) {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			rest, err := filepath.Rel(candidate, path)
			if err != nil {
				return "", err
			}
			if rest == "." {
				return filepath.Clean(resolved), nil
			}
			return filepath.Clean(filepath.Join(resolved, rest)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return path, nil
		}
	}
}
