package sandbox

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func awaitPending(t *testing.T, a *TerminalApprover) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for id := range a.Pending() {
			return id
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("approval not registered")
	return 0
}
func TestTerminalApproverInteractiveDecision(t *testing.T) {
	output := new(bytes.Buffer)
	a := NewTerminalApprover(strings.NewReader("a\n"), output)
	defer a.Close()
	a.BeginTurn()
	result, err := a.Approve(context.Background(), ApprovalRequest{Kind: ApprovalEscalation, Tool: "bash", Command: "git commit"})
	if err != nil || result.Decision != AllowSession {
		t.Fatalf("%+v %v", result, err)
	}
	if !strings.Contains(output.String(), "需要审批") {
		t.Fatal("no prompt")
	}
}
func TestTerminalApproverPendingCanBeSubmitted(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	a := NewTerminalApprover(reader, io.Discard)
	defer a.Close()
	result := make(chan ApprovalResult, 1)
	go func() {
		r, _ := a.Approve(context.Background(), ApprovalRequest{Kind: ApprovalNetwork, Host: "example.com", Port: 443})
		result <- r
	}()
	id := awaitPending(t, a)
	if err := a.Submit(id, AllowOnce, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-result:
		if r.Decision != AllowOnce {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("approval stuck")
	}
}
func TestTerminalInputRoutingAndCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	a := NewTerminalApprover(reader, io.Discard)
	defer a.Close()
	a.RouteLine("user message")
	line, err := a.NextLine(context.Background())
	if err != nil || line != "user message" {
		t.Fatalf("%s %v", line, err)
	}
	a.BeginTurn()
	a.RouteLine("discard while running")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan ApprovalResult, 1)
	go func() { r, _ := a.Approve(ctx, ApprovalRequest{}); result <- r }()
	awaitPending(t, a)
	a.RouteLine("please do this")
	select {
	case r := <-result:
		t.Fatalf("ordinary text accepted as approval: %+v", r)
	default:
	}
	cancel()
	select {
	case r := <-result:
		if r.Decision != Deny {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation stuck")
	}
	if len(a.Pending()) != 0 {
		t.Fatal("canceled approval retained")
	}
	a.EndTurn()
	short, cancelShort := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelShort()
	if _, err := a.NextLine(short); err == nil {
		t.Fatal("running text incorrectly queued as next user message")
	}
}
func TestTerminalPendingTimeoutAndRejectReason(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		a := NewTerminalApprover(reader, io.Discard)
		defer a.Close()
		a.pendingTimeout = 10 * time.Millisecond
		result, err := a.Approve(context.Background(), ApprovalRequest{Kind: ApprovalNetwork})
		if err != nil || result.Decision != Deny || result.Feedback != "审批超时" {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("reason", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		a := NewTerminalApprover(reader, io.Discard)
		defer a.Close()
		a.BeginTurn()
		result := make(chan ApprovalResult, 1)
		go func() { r, _ := a.Approve(context.Background(), ApprovalRequest{}); result <- r }()
		awaitPending(t, a)
		a.RouteLine("r")
		a.RouteLine("权限范围过大")
		if r := <-result; r.Decision != Deny || r.Feedback != "权限范围过大" {
			t.Fatal(r)
		}
	})
}
func TestTerminalNumberedAnswerDoesNotBecomeUserMessage(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	a := NewTerminalApprover(reader, io.Discard)
	defer a.Close()
	result := make(chan ApprovalResult, 1)
	go func() { r, _ := a.Approve(context.Background(), ApprovalRequest{Kind: ApprovalNetwork}); result <- r }()
	id := awaitPending(t, a)
	a.RouteLine("/approve " + strconv.Itoa(id) + " a")
	if r := <-result; r.Decision != AllowSession {
		t.Fatal(r)
	}
}

func TestCancelTurnRejectsNetworkApprovalWithIndependentContext(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	a := NewTerminalApprover(reader, io.Discard)
	defer a.Close()
	a.BeginTurn()
	result := make(chan ApprovalResult, 1)
	go func() { r, _ := a.Approve(context.Background(), ApprovalRequest{Kind: ApprovalNetwork}); result <- r }()
	awaitPending(t, a)
	a.CancelTurnApprovals()
	select {
	case r := <-result:
		if r.Decision != Deny {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("network approval survived Ctrl-C")
	}
}
