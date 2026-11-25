package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

// fakeStreamProvider 按调用顺序返回预设响应。tools 为空的调用视为 Thinking 阶段。
type fakeStreamProvider struct {
	mu            sync.Mutex
	thinking      []string // Thinking 阶段的增量
	action        []string // Action 阶段的增量
	actionErr     error
	generateCalls int
	streamCalls   int
}

func (p *fakeStreamProvider) Generate(_ context.Context, _ []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	p.mu.Lock()
	p.generateCalls++
	p.mu.Unlock()
	parts := p.action
	if tools == nil {
		parts = p.thinking
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: strings.Join(parts, "")}, nil
}

func (p *fakeStreamProvider) GenerateStream(_ context.Context, _ []schema.Message, tools []schema.ToolDefinition, onDelta provider.StreamHandler) (*schema.Message, error) {
	p.mu.Lock()
	p.streamCalls++
	p.mu.Unlock()
	parts := p.action
	if tools == nil {
		parts = p.thinking
	}
	for _, d := range parts {
		onDelta(d)
	}
	if tools != nil && p.actionErr != nil {
		return nil, p.actionErr
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: strings.Join(parts, "")}, nil
}

type recordingReporter struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingReporter) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}
func (r *recordingReporter) OnThinking(context.Context)                { r.add("thinking") }
func (r *recordingReporter) OnToolCall(_ context.Context, n, _ string) { r.add("tool:" + n) }
func (r *recordingReporter) OnToolResult(_ context.Context, n, _ string, _ bool) {
	r.add("result:" + n)
}
func (r *recordingReporter) OnMessage(_ context.Context, c string) { r.add("message:" + c) }

type recordingStreamReporter struct{ recordingReporter }

func (r *recordingStreamReporter) OnThinkingDelta(_ context.Context, d string) {
	r.add("think_delta:" + d)
}
func (r *recordingStreamReporter) OnMessageDelta(_ context.Context, d string) {
	r.add("msg_delta:" + d)
}
func (r *recordingStreamReporter) OnStreamDone(_ context.Context, err error) {
	if err != nil {
		r.add("done:err")
		return
	}
	r.add("done")
}

// 测试使用一个带工具的 registry，保证 Action 阶段 tools 非 nil，与 Thinking 区分开。
func testRegistry(t *testing.T) tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(tools.NewReadFileTool(t.TempDir()))
	return reg
}

func lastAssistant(sess *cctx.Session) schema.Message {
	msgs := sess.GetWorkingMemory(0)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.RoleAssistant {
			return msgs[i]
		}
	}
	return schema.Message{}
}

func TestRunStreamsThinkingAndActionWithoutDuplicateOnMessage(t *testing.T) {
	p := &fakeStreamProvider{thinking: []string{"想", "一想"}, action: []string{"答", "案"}}
	eng := NewAgentEngine(p, testRegistry(t), false, true)
	eng.Stream = true
	rep := &recordingStreamReporter{}
	sess := cctx.NewSession("s", t.TempDir())
	sess.Append(schema.Message{Role: schema.RoleUser, Content: "hi"})

	if err := eng.Run(context.Background(), sess, rep); err != nil {
		t.Fatal(err)
	}
	want := "thinking|think_delta:想|think_delta:一想|done|msg_delta:答|msg_delta:案|done"
	if got := strings.Join(rep.events, "|"); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	if p.generateCalls != 0 || p.streamCalls != 2 {
		t.Fatalf("generate=%d stream=%d", p.generateCalls, p.streamCalls)
	}
	if got := lastAssistant(sess).Content; got != "想一想\n答案" {
		t.Fatalf("Session 中的完整回复 = %q", got)
	}
}

func TestRunUsesGenerateWhenStreamDisabledOrReporterNotStreaming(t *testing.T) {
	cases := []struct {
		name   string
		stream bool
		rep    Reporter
	}{
		{"stream disabled", false, &recordingStreamReporter{}},
		{"reporter without stream support", true, &recordingReporter{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeStreamProvider{action: []string{"答", "案"}}
			eng := NewAgentEngine(p, testRegistry(t), false, false)
			eng.Stream = tc.stream
			sess := cctx.NewSession("s", t.TempDir())
			sess.Append(schema.Message{Role: schema.RoleUser, Content: "hi"})
			if err := eng.Run(context.Background(), sess, tc.rep); err != nil {
				t.Fatal(err)
			}
			if p.streamCalls != 0 || p.generateCalls != 1 {
				t.Fatalf("generate=%d stream=%d", p.generateCalls, p.streamCalls)
			}
			var events []string
			switch r := tc.rep.(type) {
			case *recordingStreamReporter:
				events = r.events
			case *recordingReporter:
				events = r.events
			}
			if strings.Join(events, "|") != "message:答案" {
				t.Fatalf("events = %v", events)
			}
		})
	}
}

func TestRunStreamErrorDoesNotPersistPartialReply(t *testing.T) {
	p := &fakeStreamProvider{action: []string{"半截"}, actionErr: errors.New("断开")}
	eng := NewAgentEngine(p, testRegistry(t), false, false)
	eng.Stream = true
	rep := &recordingStreamReporter{}
	sess := cctx.NewSession("s", t.TempDir())
	sess.Append(schema.Message{Role: schema.RoleUser, Content: "hi"})

	err := eng.Run(context.Background(), sess, rep)
	if err == nil || !strings.Contains(err.Error(), "Action 阶段失败") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(rep.events, "|"); got != "msg_delta:半截|done:err" {
		t.Fatalf("events = %s", got)
	}
	if msg := lastAssistant(sess); msg.Role != "" {
		t.Fatalf("半截回复不应写入 Session: %+v", msg)
	}
}

func TestTerminalReporterStreamFormatting(t *testing.T) {
	var buf bytes.Buffer
	r := &TerminalReporter{out: &buf}
	ctx := context.Background()

	r.OnThinking(ctx)
	r.OnThinkingDelta(ctx, "想")
	r.OnThinkingDelta(ctx, "法")
	r.OnStreamDone(ctx, nil)
	r.OnMessageDelta(ctx, "你")
	r.OnMessageDelta(ctx, "好")
	r.OnStreamDone(ctx, nil)
	r.OnMessageDelta(ctx, "半")
	r.OnStreamDone(ctx, errors.New("断开"))
	r.OnStreamDone(ctx, nil) // 没有增量时不输出任何东西

	want := "\n[🤔 思考中] 模型正在推理...\n想法\n" +
		"\n🤖 Agent 回复:\n你好\n\n" +
		"\n🤖 Agent 回复:\n半\n[⚠️ 输出中断] 断开\n"
	if buf.String() != want {
		t.Fatalf("输出 =\n%q\nwant\n%q", buf.String(), want)
	}
}
