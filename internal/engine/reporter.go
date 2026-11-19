// internal/engine/reporter.go
package engine

import "context"

// Reporter 定义了 Agent 引擎向外界输出信息的规范。
// engin 可以无缝切换终端 (CLI)、飞书、钉钉甚至 WebUI 等不同的展现层。
type Reporter interface {
	// OnThinking 当模型开始进行慢思考 (Reasoning) 时调用
	OnThinking(ctx context.Context)

	// OnToolCall 当模型决定并发调用工具时调用
	OnToolCall(ctx context.Context, toolName string, args string)

	// OnToolResult 当工具在底层执行完毕并返回结果时调用
	OnToolResult(ctx context.Context, toolName string, result string, isError bool)

	// OnMessage 当模型宣告任务完成，向用户输出最终纯文本回答时调用
	OnMessage(ctx context.Context, content string)
}

// StreamReporter 是可选能力：实现它的 Reporter 可以实时展示模型的流式输出。
// 引擎只在 Provider 支持流式、且 Reporter 实现了该接口时才走流式路径；
// 否则仍然只调用 OnMessage 一次性展示。
type StreamReporter interface {
	// OnThinkingDelta 在慢思考 (Thinking) 阶段收到文本增量时调用
	OnThinkingDelta(ctx context.Context, delta string)
	// OnMessageDelta 在行动 (Action) 阶段收到回复文本增量时调用。
	// 走流式路径时，引擎不会再为同一段内容调用 OnMessage。
	OnMessageDelta(ctx context.Context, delta string)
	// OnStreamDone 在一次流式生成结束时调用（无论是否产生过增量）。
	// err 非空表示流中途失败，已展示的内容不完整。
	OnStreamDone(ctx context.Context, err error)
}
