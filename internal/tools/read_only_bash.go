package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

var readOnlyCommands = map[string]bool{
	"ls": true, "pwd": true, "find": true, "grep": true, "rg": true,
	"cat": true, "head": true, "tail": true, "git": true, "go": true,
	"sed": true,
}

// NewReadOnlyBashTool adds a second, tool-level guard for subagents. The
// registry middleware remains the first guard so rejected calls are visible
// before Execute is reached.
func NewReadOnlyBashTool(workDir string) *BashTool {
	return &BashTool{workDir: workDir, readOnly: true}
}

func NewReadOnlyBashMiddleware() MiddlewareFunc {
	return func(_ context.Context, call schema.ToolCall) (bool, string) {
		if call.Name != "bash" {
			return true, ""
		}
		var args bashArgs
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return false, fmt.Sprintf("bash 参数解析失败: %v", err)
		}
		if args.Action != "" {
			return false, "子 Agent 只能执行一次性只读 bash 命令，不允许管理后台会话"
		}
		if ok, reason := validateReadOnlyCommand(args.Command); !ok {
			return false, reason
		}
		return true, ""
	}
}

func validateReadOnlyCommand(command string) (bool, string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return false, "bash 命令不能为空"
	}
	for _, r := range command {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return false, "命令包含不允许的控制字符"
		}
	}
	if strings.Contains(command, "$(") || strings.Contains(command, "${") || strings.ContainsRune(command, '`') {
		return false, "禁止命令替换或反引号"
	}
	// &&/|| are normalized to pipeline separators and each segment is checked
	// independently. A lone &, semicolon, or redirection remains forbidden.
	command = strings.ReplaceAll(command, "&&", "|")
	command = strings.ReplaceAll(command, "||", "|")
	for _, forbidden := range []string{">", "<", "&", ";", "find -exec", "find . -exec", "xargs", "awk", "system(", "sudo", "rm ", "mv ", "cp ", "mkdir", "touch", "chmod", "chown", "npm install", "npm i ", "pip install", "go install", "python -c", "python3 -c", "bash -c", "sh -c"} {
		if strings.Contains(command, forbidden) {
			return false, fmt.Sprintf("命令包含被禁止的写入或嵌套 Shell 操作: %s", forbidden)
		}
	}

	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == '|' || r == ';' || r == '\n'
	})
	for _, segment := range segments {
		fields := strings.Fields(strings.TrimSpace(segment))
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == "&&" || name == "||" {
			continue
		}
		if !readOnlyCommands[name] {
			return false, fmt.Sprintf("子 Agent 只允许只读命令，当前命令不在白名单: %s", name)
		}
		if name == "git" && len(fields) > 1 {
			switch fields[1] {
			case "status", "diff", "log", "show":
			default:
				return false, "git 只允许 status、diff、log、show"
			}
		}
		if name == "go" {
			if len(fields) < 2 || fields[1] != "env" {
				return false, "go 只允许 env"
			}
			if len(fields) > 2 && (fields[2] == "-w" || fields[2] == "-u") {
				return false, "go env 不允许修改环境配置"
			}
		}
		if name == "find" {
			for _, field := range fields[1:] {
				if field == "-delete" || field == "-exec" || field == "-execdir" || field == "-ok" || field == "-okdir" {
					return false, "find 只允许读取，禁止执行或删除动作"
				}
			}
		}
		if name == "sed" && !strings.Contains(segment, "-n") {
			return false, "sed 只允许使用 -n 读取内容"
		}
	}
	return true, ""
}
