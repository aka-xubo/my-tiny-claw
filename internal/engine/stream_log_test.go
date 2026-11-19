package engine

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/observability"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

func runWithTrackerAndTerminal(t *testing.T, p *fakeStreamProvider, thinking bool) (string, error) {
	t.Helper()
	// stdout 和日志写入同一个缓冲区，模拟终端上两者交错显示。
	var buf bytes.Buffer
	rep := &TerminalReporter{out: &buf}

	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(rep.LogWriter(&buf))
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	sess := cctx.NewSession("s", t.TempDir())
	sess.Append(schema.Message{Role: schema.RoleUser, Content: "hi"})
	tracked := observability.NewCostTracker(p, "gpt-6.1-sol", sess)
	eng := NewAgentEngine(tracked, testRegistry(t), false, thinking)
	eng.Stream = true
	err := eng.Run(context.Background(), sess, rep)
	return buf.String(), err
}

func assertNoLineMixesStreamAndLog(t *testing.T, out string, streamed ...string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "[Tracker]") && !strings.Contains(line, "[Engine]") {
			continue
		}
		for _, s := range streamed {
			if strings.Contains(line, s) {
				t.Fatalf("日志与流式文本粘在同一行: %q\n完整输出:\n%s", line, out)
			}
		}
	}
}

func TestStreamedOutputEndsLineBeforeTrackerLog(t *testing.T) {
	p := &fakeStreamProvider{thinking: []string{"想", "法"}, action: []string{"答", "案"}}
	out, err := runWithTrackerAndTerminal(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	assertNoLineMixesStreamAndLog(t, out, "想法", "答案")
	if !strings.Contains(out, "\n🤖 Agent 回复:\n答案\n") {
		t.Fatalf("回复格式异常:\n%s", out)
	}
	if strings.Contains(out, "答案\n\n\n") {
		t.Fatalf("回复后出现多余空行:\n%s", out)
	}
}

func TestStreamFailureEndsLineBeforeTrackerLog(t *testing.T) {
	p := &fakeStreamProvider{action: []string{"半截"}, actionErr: errors.New("断开")}
	out, err := runWithTrackerAndTerminal(t, p, false)
	if err == nil {
		t.Fatal("want error")
	}
	assertNoLineMixesStreamAndLog(t, out, "半截")
	if !strings.Contains(out, "[⚠️ 输出中断] 断开\n") {
		t.Fatalf("缺少中断提示:\n%s", out)
	}
}
