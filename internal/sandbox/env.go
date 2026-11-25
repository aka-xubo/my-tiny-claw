package sandbox

import (
	"path/filepath"
	"strings"
)

var builtinEnvExcludes = []string{
	"*_API_KEY",
	"*_SECRET",
	"*_SECRET_*",
	"*_TOKEN",
	"*PASSWORD*",
	"CODEX_API_KEY",
	"FEISHU_*",
}

func CleanEnvironment(env []string, exclude, keep []string) []string {
	keepSet := make(map[string]struct{}, len(keep))
	for _, pattern := range keep {
		keepSet[strings.ToUpper(pattern)] = struct{}{}
	}
	patterns := append(append([]string{}, builtinEnvExcludes...), exclude...)
	result := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(name)
		if matchesAny(upper, keepSet) || !matchesAnyPattern(upper, patterns) {
			result = append(result, entry)
		}
	}
	return result
}

func matchesAny(value string, patterns map[string]struct{}) bool {
	for pattern := range patterns {
		if ok, _ := filepath.Match(pattern, value); ok || pattern == value {
			return true
		}
	}
	return false
}

func matchesAnyPattern(value string, patterns []string) bool {
	for _, pattern := range patterns {
		if ok, _ := filepath.Match(strings.ToUpper(pattern), value); ok {
			return true
		}
	}
	return false
}
