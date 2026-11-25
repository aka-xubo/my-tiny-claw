package sandbox

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// GenerateSBPL is the human-readable policy preview. Execution uses
// CompileSBPL, which passes every literal path using sandbox-exec -D.
func GenerateSBPL(profile *Profile) (string, error) { return GenerateSBPLWithProxy(profile, nil) }
func GenerateSBPLWithProxy(profile *Profile, proxy *ProxyEndpoints) (string, error) {
	text, _, err := compileSBPL(profile, proxy, false)
	return text, err
}
func CompileSBPL(profile *Profile, proxy *ProxyEndpoints) (string, []string, error) {
	return compileSBPL(profile, proxy, true)
}

func compileSBPL(profile *Profile, proxy *ProxyEndpoints, params bool) (string, []string, error) {
	if profile == nil {
		return "", nil, fmt.Errorf("权限档不能为空")
	}
	if profile.FullAccess {
		return "", nil, nil
	}
	if profile.Network < NetworkDisabled || profile.Network > NetworkFull {
		return "", nil, fmt.Errorf("未知网络模式")
	}
	rules, err := canonicalRules(profile.Files)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	b.WriteString(`(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target self))
(allow signal (target same-sandbox))
(allow sysctl-read)
(allow file-read-metadata)
(allow mach-lookup (global-name "com.apple.system.opendirectoryd.libinfo"))
(allow ipc-posix-sem)
(allow ipc-posix-shm)
(allow pseudo-tty)
(allow file-read* file-write* (literal "/dev/null") (literal "/dev/zero") (literal "/dev/tty"))
`)
	args := []string{}
	pathExpr := func(path string) string {
		if !params {
			return fmt.Sprintf("\"%s\"", sbplString(path))
		}
		name := fmt.Sprintf("PATH_%d", len(args)/2)
		args = append(args, "-D", name+"="+path)
		return fmt.Sprintf("(param \"%s\")", name)
	}
	predicates := make([]string, len(rules))
	for i, rule := range rules {
		if rule.Glob != "" {
			expression, err := GlobToRegex(rule.Path, rule.Glob)
			if err != nil {
				return "", nil, err
			}
			// Seatbelt accepts regex strings through param as well. Avoid raw
			// #"..." literals, whose quoting cannot safely encode all paths.
			predicates[i] = "(regex " + pathExpr(expression) + ")"
		} else {
			predicates[i] = "(subpath " + pathExpr(rule.Path) + ")"
		}
	}
	// Every allow excludes all higher-priority rules. No overlapping explicit
	// deny is needed, so permissions do not depend on SBPL source order.
	for i, rule := range rules {
		if rule.Access == AccessDeny {
			continue
		}
		terms := []string{predicates[i]}
		for j, higher := range rules {
			if higherPriority(higher, rule) {
				terms = append(terms, "(require-not "+predicates[j]+")")
			}
		}
		filter := terms[0]
		if len(terms) > 1 {
			filter = "(require-all " + strings.Join(terms, " ") + ")"
		}
		ops := "file-read*"
		if rule.Access == AccessWrite {
			ops += " file-write*"
		}
		fmt.Fprintf(&b, "(allow %s %s)\n", ops, filter)
	}
	roots := append([]string(nil), profile.ProtectedRoots...)
	sort.Strings(roots)
	for _, root := range roots {
		fmt.Fprintf(&b, "(deny file-write-unlink (literal %s))\n", pathExpr(root))
	}
	switch profile.Network {
	case NetworkFull:
		b.WriteString("(allow network*)\n(allow system-socket)\n(allow mach-lookup (global-name \"com.apple.mDNSResponder\"))\n")
	case NetworkProxy:
		if proxy != nil {
			if proxy.HTTPPort < 1 || proxy.HTTPPort > 65535 || proxy.SOCKSPort < 1 || proxy.SOCKSPort > 65535 {
				return "", nil, fmt.Errorf("代理端口非法")
			}
			fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\") (remote ip \"localhost:%d\"))\n", proxy.HTTPPort, proxy.SOCKSPort)
		}
	}
	return b.String(), args, nil
}

func canonicalRules(input []FileRule) ([]FileRule, error) {
	byKey := map[string]FileRule{}
	for _, rule := range input {
		if !filepath.IsAbs(rule.Path) || strings.ContainsAny(rule.Path, "\x00\r\n") {
			return nil, fmt.Errorf("权限路径必须是合法绝对路径: %q", rule.Path)
		}
		if rule.Access < AccessDeny || rule.Access > AccessWrite {
			return nil, fmt.Errorf("未知文件权限")
		}
		if err := ValidateGlob(rule.Glob); err != nil {
			return nil, err
		}
		if rule.Approved && rule.Glob != "" {
			return nil, fmt.Errorf("额外权限必须使用字面路径")
		}
		key := fmt.Sprintf("%s\x00%s\x00%t", rule.Path, rule.Glob, rule.Approved)
		old, ok := byKey[key]
		if !ok || rule.Approved && !old.Approved || rule.Approved == old.Approved && ((!rule.Approved && rule.Access < old.Access) || (rule.Approved && rule.Access > old.Access)) {
			byKey[key] = rule
		}
	}
	result := make([]FileRule, 0, len(byKey))
	for _, rule := range byKey {
		result = append(result, rule)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		if result[i].Glob != result[j].Glob {
			return result[i].Glob < result[j].Glob
		}
		return !result[i].Approved && result[j].Approved
	})
	return result, nil
}

func higherPriority(a, b FileRule) bool {
	if a.Approved && b.Approved {
		return a.Access > b.Access
	}
	if a.Approved != b.Approved {
		return a.Approved && !(a.Access == AccessRead && b.Access == AccessWrite)
	}
	if (a.Glob != "") != (b.Glob != "") {
		return a.Glob != ""
	}
	if a.Glob != "" {
		return a.Access < b.Access
	}
	return isStrictChild(b.Path, a.Path)
}
func isStrictChild(parent, child string) bool {
	return parent != child && strings.HasPrefix(child, strings.TrimRight(parent, "/")+"/")
}
func isStrictChildOrEqual(parent, child string) bool {
	return parent == child || isStrictChild(parent, child)
}
func sbplString(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value)
}
