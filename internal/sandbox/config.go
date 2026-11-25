package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aka-xubo/my-tiny-claw/internal/sandbox/proxy"
	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Profiles           map[string]*Profile
	DefaultPermissions string
	ApprovalPolicy     ApprovalPolicy
	EnvExclude         []string
	EnvKeep            []string
}

func LoadConfig(workspace string) (*Config, error) {
	workspace, err := NormalizePath(workspace)
	if err != nil {
		return nil, err
	}
	profiles, err := BuiltinProfiles(workspace)
	if err != nil {
		return nil, err
	}
	merged := map[string]any{}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	userPath := filepath.Join(home, ".claw", "config.toml")
	user, err := readTOMLIfExists(userPath)
	if err != nil {
		return nil, fmt.Errorf("读取用户配置失败: %w", err)
	}
	mergeMaps(merged, user)
	projectPath := filepath.Join(workspace, ".claw", "config.toml")
	if projectTrusted(user, workspace) {
		project, err := readTOMLIfExists(projectPath)
		if err != nil {
			return nil, fmt.Errorf("读取项目配置失败: %w", err)
		}
		mergeMaps(merged, project)
	}
	return buildConfig(workspace, profiles, merged)
}

func ProjectConfigStatus(workspace string) (exists, trusted bool, err error) {
	workspace, err = NormalizePath(workspace)
	if err != nil {
		return false, false, err
	}
	projectPath := filepath.Join(workspace, ".claw", "config.toml")
	if _, err := os.Stat(projectPath); errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return true, false, err
	}
	user, err := readTOMLIfExists(filepath.Join(home, ".claw", "config.toml"))
	if err != nil {
		return true, false, err
	}
	return true, projectTrusted(user, workspace), nil
}

func TrustProject(workspace string) error {
	workspace, err := NormalizePath(workspace)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configDir := filepath.Join(home, ".claw")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(configDir, "config.toml")
	raw, err := readTOMLIfExists(path)
	if err != nil {
		return err
	}
	projects, _ := asMap(raw["projects"])
	if projects == nil {
		projects = make(map[string]any)
		raw["projects"] = projects
	}
	projects[workspace] = map[string]any{"trust_level": "trusted"}
	encoded, err := toml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0600)
}

func buildConfig(workspace string, profiles map[string]*Profile, raw map[string]any) (*Config, error) {
	if err := validateConfigTypes(raw); err != nil {
		return nil, err
	}
	config := &Config{Profiles: profiles, DefaultPermissions: ":workspace", ApprovalPolicy: ApprovalOnRequest}
	if value, ok := raw["default_permissions"].(string); ok && value != "" {
		config.DefaultPermissions = value
	}
	if value, ok := raw["approval_policy"].(string); ok && value != "" {
		config.ApprovalPolicy = ApprovalPolicy(value)
	}
	if config.ApprovalPolicy != ApprovalUntrusted && config.ApprovalPolicy != ApprovalOnRequest && config.ApprovalPolicy != ApprovalNever {
		return nil, fmt.Errorf("未知审批策略: %s", config.ApprovalPolicy)
	}
	if env, ok := asMap(raw["shell_environment"]); ok {
		config.EnvExclude = asStringSlice(env["exclude"])
		config.EnvKeep = asStringSlice(env["keep"])
	}
	rawProfiles, _ := asMap(raw["permissions"])
	building := make(map[string]bool)
	depth := 0
	var build func(string) (*Profile, error)
	build = func(name string) (*Profile, error) {
		if profile, ok := profiles[name]; ok && strings.HasPrefix(name, ":") {
			return cloneProfile(profile), nil
		}
		if building[name] {
			return nil, fmt.Errorf("权限档 extends 出现循环: %s", name)
		}
		if depth >= 8 {
			return nil, fmt.Errorf("权限档 extends 深度超过 8: %s", name)
		}
		rawValue, ok := rawProfiles[name]
		if !ok {
			return nil, fmt.Errorf("权限档不存在: %s", name)
		}
		item, ok := asMap(rawValue)
		if !ok {
			return nil, fmt.Errorf("权限档格式错误: %s", name)
		}
		building[name] = true
		depth++
		defer delete(building, name)
		defer func() { depth-- }()
		profile := &Profile{Name: name, Network: NetworkDisabled}
		if extends, _ := item["extends"].(string); extends != "" {
			base, err := build(extends)
			if err != nil {
				return nil, err
			}
			profile = base
			profile.Name = name
		}
		if description, _ := item["description"].(string); description != "" {
			profile.Description = description
		}
		if filesystem, ok := asMap(item["filesystem"]); ok {
			for rawPath, rawAccess := range filesystem {
				if rawPath == ":workspace_roots" {
					children, ok := asMap(rawAccess)
					if !ok {
						return nil, fmt.Errorf(":workspace_roots 必须是表")
					}
					for relative, access := range children {
						if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(filepath.Clean(relative), ".."+string(filepath.Separator)) {
							return nil, fmt.Errorf("工作区规则不能越界: %s", relative)
						}
						path := filepath.Join(workspace, relative)
						if err := addFileRule(profile, path, access); err != nil {
							return nil, err
						}
					}
					continue
				}
				if err := addFileRule(profile, rawPath, rawAccess); err != nil {
					return nil, err
				}
			}
		}
		if network, ok := asMap(item["network"]); ok {
			if mode, _ := network["mode"].(string); mode != "" {
				switch mode {
				case "disabled":
					profile.Network = NetworkDisabled
				case "proxy":
					profile.Network = NetworkProxy
				case "full":
					profile.Network = NetworkFull
				default:
					return nil, fmt.Errorf("未知网络模式: %s", mode)
				}
			}
			if domains, ok := asMap(network["domains"]); ok {
				if profile.Domains == nil {
					profile.Domains = make(map[string]DomainRule)
				}
				for domain, rawRule := range domains {
					rule, _ := rawRule.(string)
					if rule != string(DomainAllow) && rule != string(DomainDeny) {
						return nil, fmt.Errorf("未知域名规则: %s = %s", domain, rule)
					}
					profile.Domains[domain] = DomainRule(rule)
				}
			}
		}
		if profile.FullAccess {
			return nil, fmt.Errorf("自定义档不能继承完全访问档后追加规则: %s", name)
		}
		domains := make(proxy.Rules, len(profile.Domains))
		for k, v := range profile.Domains {
			domains[k] = proxy.DomainRule(v)
		}
		normalized, err := proxy.NormalizeRules(domains)
		if err != nil {
			return nil, err
		}
		profile.Domains = make(map[string]DomainRule, len(normalized))
		for k, v := range normalized {
			profile.Domains[k] = DomainRule(v)
		}
		if _, err := canonicalRules(profile.Files); err != nil {
			return nil, err
		}
		profiles[name] = profile
		return cloneProfile(profile), nil
	}
	for name := range rawProfiles {
		if strings.HasPrefix(name, ":") {
			return nil, fmt.Errorf("内置权限档不可覆盖: %s", name)
		}
		if _, err := build(name); err != nil {
			return nil, err
		}
	}
	if _, ok := profiles[config.DefaultPermissions]; !ok {
		return nil, fmt.Errorf("默认权限档不存在: %s", config.DefaultPermissions)
	}
	return config, nil
}

func addFileRule(profile *Profile, rawPath string, rawAccess any) error {
	accessName, ok := rawAccess.(string)
	if !ok {
		return fmt.Errorf("文件权限格式错误: %s", rawPath)
	}
	access := AccessDeny
	switch accessName {
	case "read":
		access = AccessRead
	case "write":
		access = AccessWrite
	case "deny":
	default:
		return fmt.Errorf("未知文件权限: %s = %s", rawPath, accessName)
	}
	path := rawPath
	if path == ":root" {
		path = "/"
	}
	if path == ":tmpdir" {
		path = os.TempDir()
	}
	if !filepath.IsAbs(path) && path != "~" && !strings.HasPrefix(path, "~/") {
		return fmt.Errorf("文件规则必须是绝对路径或受支持的占位符: %s", rawPath)
	}
	if err := ValidateGlob(path); err != nil {
		return err
	}
	if strings.ContainsAny(path, "*?") {
		if err := ValidateGlob(path); err != nil {
			return err
		}
		wildcard := strings.IndexAny(path, "*?")
		prefix := path[:wildcard]
		separator := strings.LastIndex(prefix, string(filepath.Separator))
		if separator < 0 {
			return fmt.Errorf("glob 缺少根目录")
		}
		root := prefix[:separator]
		if root == "" {
			root = "/"
		}
		if root == "." || root == "" {
			return fmt.Errorf("glob 必须位于绝对路径或 :workspace_roots 下: %s", rawPath)
		}
		glob := strings.TrimPrefix(strings.TrimPrefix(path[len(root):], string(filepath.Separator)), "/")
		normalized, err := NormalizePath(root)
		if err != nil {
			return err
		}
		profile.Files = append(profile.Files, FileRule{Path: normalized, Glob: glob, Access: access})
		return nil
	}
	normalized, err := NormalizePath(path)
	if err != nil {
		return err
	}
	profile.Files = append(profile.Files, FileRule{Path: normalized, Access: access})
	return nil
}

// Reject malformed values rather than silently replacing them with defaults.
func validateConfigTypes(raw map[string]any) error {
	for _, key := range []string{"default_permissions", "approval_policy"} {
		if v, ok := raw[key]; ok {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s 必须是字符串", key)
			}
		}
	}
	for _, key := range []string{"permissions", "shell_environment", "projects"} {
		if v, ok := raw[key]; ok {
			if _, ok := asMap(v); !ok {
				return fmt.Errorf("%s 必须是表", key)
			}
		}
	}
	if env, ok := asMap(raw["shell_environment"]); ok {
		for _, key := range []string{"exclude", "keep"} {
			if v, ok := env[key]; ok {
				items, ok := v.([]any)
				if !ok {
					if _, ok := v.([]string); ok {
						continue
					}
					return fmt.Errorf("%s 必须是字符串数组", key)
				}
				for _, item := range items {
					if _, ok := item.(string); !ok {
						return fmt.Errorf("%s 必须是字符串数组", key)
					}
				}
			}
		}
	}
	if profiles, ok := asMap(raw["permissions"]); ok {
		for name, v := range profiles {
			p, ok := asMap(v)
			if !ok {
				return fmt.Errorf("权限档 %s 必须是表", name)
			}
			for _, key := range []string{"extends", "description"} {
				if v, ok := p[key]; ok {
					if _, ok := v.(string); !ok {
						return fmt.Errorf("%s.%s 必须是字符串", name, key)
					}
				}
			}
			for _, key := range []string{"filesystem", "network"} {
				if v, ok := p[key]; ok {
					if _, ok := asMap(v); !ok {
						return fmt.Errorf("%s.%s 必须是表", name, key)
					}
				}
			}
			if net, ok := asMap(p["network"]); ok {
				if v, ok := net["mode"]; ok {
					if _, ok := v.(string); !ok {
						return fmt.Errorf("network.mode 必须是字符串")
					}
				}
				if v, ok := net["domains"]; ok {
					if _, ok := asMap(v); !ok {
						return fmt.Errorf("network.domains 必须是表")
					}
				}
			}
		}
	}
	return nil
}

func readTOMLIfExists(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]any)
	if err := toml.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func projectTrusted(user map[string]any, workspace string) bool {
	projects, ok := asMap(user["projects"])
	if !ok {
		return false
	}
	project, ok := asMap(projects[workspace])
	if !ok {
		return false
	}
	trust, _ := project["trust_level"].(string)
	return trust == "trusted"
}

func mergeMaps(dst, src map[string]any) {
	for key, value := range src {
		srcMap, srcOK := asMap(value)
		dstMap, dstOK := asMap(dst[key])
		if srcOK && dstOK {
			mergeMaps(dstMap, srcMap)
			continue
		}
		dst[key] = value
	}
}

func asMap(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func asStringSlice(value any) []string {
	if strings, ok := value.([]string); ok {
		return append([]string(nil), strings...)
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
