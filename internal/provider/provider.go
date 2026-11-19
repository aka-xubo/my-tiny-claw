package provider

import (
	"context"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

// LLMProvider defines the unified interface for communicating with large models
type LLMProvider interface {
	// Generate receives the current context history and available tools list, returns the model response
	Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error)
}

// StreamHandler 接收模型逐步生成的文本增量。
type StreamHandler func(delta string)

// StreamingProvider 是可选能力：支持流式输出的 Provider 额外实现它。
// GenerateStream 在生成过程中把文本增量交给 onDelta，结束后返回与 Generate
// 语义一致的完整消息（内容、ToolCalls、Usage）。
type StreamingProvider interface {
	LLMProvider
	GenerateStream(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition, onDelta StreamHandler) (*schema.Message, error)
}
