// internal/engine/terminal_reporter.go
package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// TerminalReporter 实现了 Reporter 接口，用于在终端直观地打印 Agent 的状态
type TerminalReporter struct {
	out io.Writer

	mu sync.Mutex
	// streaming 记录当前正在输出的流类型（"thinking" / "message"），空表示没有流在输出
	streaming string
	// midLine 表示流式文本停在行中间（最后一个增量不以换行结尾）
	midLine bool
}

const (
	streamThinking = "thinking"
	streamMessage  = "message"
)

func NewTerminalReporter() *TerminalReporter {
	return &TerminalReporter{out: os.Stdout}
}

func (r *TerminalReporter) writer() io.Writer {
	if r.out == nil {
		return os.Stdout
	}
	return r.out
}

func (r *TerminalReporter) OnThinking(ctx context.Context) {
	fmt.Fprintf(r.writer(), "\n[🤔 思考中] 模型正在推理...\n")
}

func (r *TerminalReporter) OnToolCall(ctx context.Context, toolName string, args string) {
	fmt.Fprintf(r.writer(), "[🛠️ 调用工具] %s\n", toolName)
	// 截断过长的参数显示，保持终端清爽
	displayArgs := strings.ReplaceAll(args, "\n", "\\n")
	displayArgs = strings.ReplaceAll(displayArgs, "\r", "\\r")
	if len(displayArgs) > 150 {
		displayArgs = displayArgs[:150] + "... (已截断)"
	}
	fmt.Fprintf(r.writer(), "   参数: %s\n", displayArgs)
}

func (r *TerminalReporter) OnToolResult(ctx context.Context, toolName string, result string, isError bool) {
	if isError {
		fmt.Fprintf(r.writer(), "[❌ 执行失败] %s\n", toolName)
		// 显示错误信息
		if result != "" {
			fmt.Fprintf(r.writer(), "   错误: %s\n", result)
		}
	} else {
		fmt.Fprintf(r.writer(), "[✅ 执行成功] %s\n", toolName)
	}
}

func (r *TerminalReporter) OnMessage(ctx context.Context, content string) {
	if content == "" {
		return
	}
	fmt.Fprintf(r.writer(), "\n🤖 Agent 回复:\n%s\n\n", content)
}

// OnThinkingDelta 紧跟在 OnThinking 打印的提示之后，直接输出思考内容的增量
func (r *TerminalReporter) OnThinkingDelta(ctx context.Context, delta string) {
	r.writeDelta(streamThinking, "", delta)
}

// OnMessageDelta 在第一个增量到达时打印回复抬头，之后原样输出增量
func (r *TerminalReporter) OnMessageDelta(ctx context.Context, delta string) {
	r.writeDelta(streamMessage, "\n🤖 Agent 回复:\n", delta)
}

func (r *TerminalReporter) writeDelta(kind, header, delta string) {
	if delta == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streaming != kind {
		r.endLineLocked()
		r.streaming = kind
		fmt.Fprint(r.writer(), header)
	}
	fmt.Fprint(r.writer(), delta)
	r.midLine = !strings.HasSuffix(delta, "\n")
}

// endLineLocked 在流式文本停在行中间时补一个换行。调用方需持有 r.mu。
func (r *TerminalReporter) endLineLocked() {
	if r.midLine {
		fmt.Fprint(r.writer(), "\n")
		r.midLine = false
	}
}

func (r *TerminalReporter) OnStreamDone(ctx context.Context, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endLineLocked()
	if err != nil {
		fmt.Fprintf(r.writer(), "[⚠️ 输出中断] %v\n", err)
	} else if r.streaming == streamMessage {
		fmt.Fprint(r.writer(), "\n") // 回复结束后空一行
	}
	r.streaming = ""
}

// LogWriter 包装日志输出（通常是 os.Stderr）。写日志前，如果流式文本停在行中间，
// 先在终端补一个换行，避免 [Tracker] 等日志粘在流式文本末尾。
// 用法：log.SetOutput(reporter.LogWriter(os.Stderr))
func (r *TerminalReporter) LogWriter(w io.Writer) io.Writer {
	return &lineAwareLogWriter{r: r, w: w}
}

type lineAwareLogWriter struct {
	r *TerminalReporter
	w io.Writer
}

func (l *lineAwareLogWriter) Write(p []byte) (int, error) {
	l.r.mu.Lock()
	defer l.r.mu.Unlock()
	l.r.endLineLocked()
	return l.w.Write(p)
}

var _ Reporter = (*TerminalReporter)(nil)
var _ StreamReporter = (*TerminalReporter)(nil)
