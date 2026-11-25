package tools

import (
	"fmt"

	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
)

type sandboxToolArgs struct {
	SandboxPermissions    sandbox.SandboxPermission `json:"sandbox_permissions"`
	Justification         string                    `json:"justification"`
	AdditionalPermissions *additionalPermissionsArg `json:"additional_permissions"`
}

type additionalPermissionsArg struct {
	Filesystem map[string]string `json:"filesystem"`
	Network    string            `json:"network"`
}

func (a sandboxToolArgs) call(tool, command, dir, operation, path string) (sandbox.CallPermissions, error) {
	call := sandbox.CallPermissions{
		Tool:               tool,
		Command:            command,
		Dir:                dir,
		Operation:          operation,
		Path:               path,
		SandboxPermissions: a.SandboxPermissions,
		Justification:      a.Justification,
	}
	if a.AdditionalPermissions == nil {
		return call, nil
	}
	additional := &sandbox.AdditionalPermissions{Filesystem: make(map[string]sandbox.Access)}
	for rawPath, rawAccess := range a.AdditionalPermissions.Filesystem {
		switch rawAccess {
		case "read":
			additional.Filesystem[rawPath] = sandbox.AccessRead
		case "write":
			additional.Filesystem[rawPath] = sandbox.AccessWrite
		default:
			return sandbox.CallPermissions{}, fmt.Errorf("additional_permissions.filesystem[%q] 必须是 read 或 write", rawPath)
		}
	}
	switch a.AdditionalPermissions.Network {
	case "", "disabled":
		additional.Network = sandbox.NetworkDisabled
	case "proxy":
		additional.Network = sandbox.NetworkProxy
	case "full":
		additional.Network = sandbox.NetworkFull
	default:
		return sandbox.CallPermissions{}, fmt.Errorf("additional_permissions.network 必须是 disabled、proxy 或 full")
	}
	call.AdditionalPermissions = additional
	return call, nil
}

func addSandboxProperties(properties map[string]interface{}) {
	properties["sandbox_permissions"] = map[string]interface{}{
		"type":        "string",
		"enum":        []string{"use_default", "with_additional_permissions", "require_escalated"},
		"description": "本次调用使用默认权限、申请额外权限，或申请完全提权",
	}
	properties["justification"] = map[string]interface{}{
		"type":        "string",
		"description": "申请额外权限或完全提权时说明理由",
	}
	properties["additional_permissions"] = map[string]interface{}{
		"type":        "object",
		"description": "with_additional_permissions 时要增加的文件和网络权限",
		"properties": map[string]interface{}{
			"filesystem": map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string", "enum": []string{"read", "write"}}},
			"network":    map[string]interface{}{"type": "string", "enum": []string{"disabled", "proxy", "full"}},
		},
	}
}
