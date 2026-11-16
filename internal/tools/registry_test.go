package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aka-xubo/my-tiny-claw/internal/observability"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

type fakeTool struct {
	name   string
	redact bool
}

func (f *fakeTool) Name() string { return f.name }
func (f *fakeTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: f.name}
}
func (f *fakeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "secret-output", nil
}

type redactingTool struct{ fakeTool }

func (r *redactingTool) RedactTrace() bool { return true }

func TestGetAvailableToolsSortedByName(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"zeta", "alpha", "mcp__b__x", "bash"} {
		r.Register(&fakeTool{name: n})
	}
	want := []string{"alpha", "bash", "mcp__b__x", "zeta"}
	for i := 0; i < 5; i++ {
		defs := r.GetAvailableTools()
		for j, d := range defs {
			if d.Name != want[j] {
				t.Fatalf("第 %d 次调用顺序错误: got %v", i, defs)
			}
		}
	}
}

func TestExecuteRedactsTraceForTraceRedactor(t *testing.T) {
	r := NewRegistry()
	r.Register(&redactingTool{fakeTool{name: "mcp__s__t"}})
	r.Register(&fakeTool{name: "plain"})

	ctx, root := observability.StartSpan(context.Background(), "root")
	args := json.RawMessage(`{"token":"secret-arg"}`)
	r.Execute(ctx, schema.ToolCall{ID: "1", Name: "mcp__s__t", Arguments: args})
	r.Execute(ctx, schema.ToolCall{ID: "2", Name: "plain", Arguments: args})

	if len(root.Children) != 2 {
		t.Fatalf("期望 2 个子 Span, got %d", len(root.Children))
	}
	redacted, plain := root.Children[0].Attributes, root.Children[1].Attributes
	if _, ok := redacted["arguments"]; ok {
		t.Fatalf("脱敏工具不应记录 arguments: %v", redacted)
	}
	if _, ok := redacted["output_preview"]; ok {
		t.Fatalf("脱敏工具不应记录 output_preview: %v", redacted)
	}
	if redacted["arguments_bytes"] != len(args) {
		t.Fatalf("脱敏工具应记录参数字节数: %v", redacted)
	}
	if plain["arguments"] != string(args) || plain["output_preview"] != "secret-output" {
		t.Fatalf("普通工具 Trace 行为应保持不变: %v", plain)
	}
}
