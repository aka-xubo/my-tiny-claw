// internal/mcpclient/remote_tool.go
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RemoteTool 是 MCP 远程工具在本地 Registry 中的代理，实现 tools.BaseTool
type RemoteTool struct {
	name        string // 暴露给模型的名称
	server      string
	remoteName  string
	description string
	inputSchema map[string]any
	session     *mcp.ClientSession
	callTimeout time.Duration
}

func (t *RemoteTool) Name() string { return t.name }

func (t *RemoteTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.name,
		Description: t.description,
		InputSchema: t.inputSchema,
	}
}

// RedactTrace 外部工具的参数和输出不写入 Trace
func (t *RemoteTool) RedactTrace() bool { return true }

func (t *RemoteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		trimmed = []byte("{}")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return "", fmt.Errorf("参数必须是 JSON 对象: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, t.callTimeout)
	defer cancel()
	res, err := t.session.CallTool(callCtx, &mcp.CallToolParams{
		Name:      t.remoteName,
		Arguments: json.RawMessage(trimmed),
	})
	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return "", fmt.Errorf("MCP 工具 %s/%s 调用超时 (%s)", t.server, t.remoteName, t.callTimeout)
		}
		return "", fmt.Errorf("MCP 工具 %s/%s 调用失败: %w", t.server, t.remoteName, err)
	}
	output := formatResult(res)
	if res.IsError {
		return "", fmt.Errorf("MCP 工具 %s/%s 返回错误: %s", t.server, t.remoteName, output)
	}
	return output, nil
}
