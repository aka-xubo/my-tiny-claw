package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/engine"
	"github.com/aka-xubo/my-tiny-claw/internal/fshelper"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__fs-helper" {
		if fshelper.Serve(os.Stdin, os.Stdout) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Tests the complete REPL/engine/tool/Seatbelt path with deterministic model
// calls so validation does not depend on API availability or model choices.
type scriptedProvider struct {
	mu              sync.Mutex
	step            int
	gitDir          string
	approvedRequest chan struct{}
	failureObserved bool
}

func (p *scriptedProvider) Generate(ctx context.Context, messages []schema.Message, defs []schema.ToolDefinition) (*schema.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	step := p.step
	p.step++
	switch step {
	case 0:
		args, _ := json.Marshal(map[string]any{"path": "code.txt", "content": "code"})
		return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "write", Name: "write_file", Arguments: args}}}, nil
	case 1:
		return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "default-commit", Name: "bash", Arguments: json.RawMessage(`{"command":"git add code.txt"}`)}}}, nil
	case 2:
		for _, m := range messages {
			if strings.Contains(m.Content, "Operation not permitted") {
				p.failureObserved = true
			}
		}
		args, _ := json.Marshal(map[string]any{"command": "git add code.txt && git -c core.hooksPath=/dev/null -c commit.gpgsign=false -c user.name=Sandbox -c user.email=sandbox@example.invalid commit -m test", "sandbox_permissions": "with_additional_permissions", "justification": "提交临时测试仓库", "additional_permissions": map[string]any{"filesystem": map[string]string{p.gitDir: "write"}}})
		close(p.approvedRequest)
		return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "approved-commit", Name: "bash", Arguments: args}}}, nil
	default:
		return &schema.Message{Role: schema.RoleAssistant, Content: "done"}, nil
	}
}
func requireRealSeatbelt(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS")
	}
	out, err := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput()
	if err != nil {
		if os.Getenv("CLAW_REQUIRE_SEATBELT") != "1" && strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
			t.Skip("outer sandbox")
		}
		t.Fatalf("%s %v", out, err)
	}
}
func TestREPLWriteApproveAndCommitWithSeatbelt(t *testing.T) {
	requireRealSeatbelt(t)
	ws := t.TempDir()
	out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput()
	if err != nil {
		t.Fatalf("git init %s %v", out, err)
	}
	profiles, err := sandbox.BuiltinProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	terminal := sandbox.NewTerminalApprover(reader, io.Discard)
	defer terminal.Close()
	session, err := sandbox.NewSession(profiles[":workspace"], sandbox.ApprovalOnRequest, terminal, []string{ws})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	manager := tools.NewTaskManager()
	defer manager.Shutdown()
	reg := tools.NewRegistry()
	reg.Register(tools.NewWriteFileToolWithSandbox(ws, session))
	reg.Register(tools.NewManagedBashToolWithSandbox(ws, manager, session))
	provider := &scriptedProvider{gitDir: filepath.Join(ws, ".git"), approvedRequest: make(chan struct{})}
	eng := engine.NewAgentEngine(provider, reg, false, false)
	sess := cctx.NewSession("acceptance", ws)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runREPL(ctx, terminal, eng, nil, sess, session, profiles, manager) }()
	if _, err := io.WriteString(writer, "修改并提交\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.approvedRequest:
	case <-ctx.Done():
		t.Fatal("no permission request")
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(terminal.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(terminal.Pending()) == 0 {
		t.Fatal("approval not displayed")
	}
	if _, err := io.WriteString(writer, "y\n"); err != nil {
		t.Fatal(err)
	}
	// Wait for the actual commit rather than sending /exit during an active turn.
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(ws, ".git", "refs", "heads", "master")); err == nil {
			break
		}
		if out, err := exec.Command("git", "-C", ws, "rev-parse", "--verify", "HEAD").Output(); err == nil && len(out) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		provider.mu.Lock()
		step := provider.step
		provider.mu.Unlock()
		if step >= 4 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("turn did not finish")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	waitForPrompt(t, terminal, ctx)
	_, _ = io.WriteString(writer, "/exit\n")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("REPL did not exit")
	}
	provider.mu.Lock()
	observed := provider.failureObserved
	provider.mu.Unlock()
	if !observed {
		t.Fatal("default git write was not denied before approval")
	}
	out, err = exec.Command("git", "-C", ws, "log", "-1", "--format=%s").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "test" {
		t.Fatalf("commit missing: %s %v", out, err)
	}
}

type cancelProvider struct {
	once        sync.Once
	started     chan struct{}
	afterCancel chan struct{}
	calls       int
	mu          sync.Mutex
}

func (p *cancelProvider) Generate(ctx context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		close(p.started)
		<-ctx.Done()
		close(p.afterCancel)
		return nil, ctx.Err()
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "second turn"}, nil
}
func TestREPLCtrlCCancelsOnlyCurrentTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix signals")
	}
	ws := t.TempDir()
	profiles, err := sandbox.BuiltinProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	terminal := sandbox.NewTerminalApprover(reader, io.Discard)
	defer terminal.Close()
	session, _ := sandbox.NewSession(profiles[":workspace"], sandbox.ApprovalOnRequest, terminal, []string{ws})
	defer session.Close()
	manager := tools.NewTaskManager()
	defer manager.Shutdown()
	p := &cancelProvider{started: make(chan struct{}), afterCancel: make(chan struct{})}
	eng := engine.NewAgentEngine(p, tools.NewRegistry(), false, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runREPL(ctx, terminal, eng, nil, cctx.NewSession("ctrl-c", ws), session, profiles, manager)
	}()
	_, _ = io.WriteString(writer, "first\n")
	select {
	case <-p.started:
	case <-ctx.Done():
		t.Fatal("turn not started")
	}
	process, _ := os.FindProcess(os.Getpid())
	if err := process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.afterCancel:
	case <-ctx.Done():
		t.Fatal("Ctrl-C did not cancel turn")
	}
	waitForPrompt(t, terminal, ctx)
	_, _ = io.WriteString(writer, "second\n")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := p.calls
		p.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	p.mu.Lock()
	calls := p.calls
	p.mu.Unlock()
	if calls < 2 {
		t.Fatal("second turn was not processed")
	}
	waitForPrompt(t, terminal, ctx)
	_, _ = io.WriteString(writer, "/exit\n")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(fmt.Errorf("REPL did not exit: %w", ctx.Err()))
	}
}

func waitForPrompt(t *testing.T, terminal *sandbox.TerminalApprover, ctx context.Context) {
	t.Helper()
	for terminal.InTurn() {
		select {
		case <-ctx.Done():
			t.Fatal("prompt not restored")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
