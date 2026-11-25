package tools

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
)

// Exercise the real synchronous execution path without requiring Seatbelt.
// Filesystem/network enforcement remains covered by the integration tests.
func TestReadOnlyOutputPreservesTailWithoutSeatbelt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires /bin/bash")
	}
	for _, test := range []struct {
		name    string
		command string
		timeout bool
		failure bool
	}{
		{"success", "seq 1 30000; printf TAIL_MARKER", false, false},
		{"failure", "seq 1 30000; printf TAIL_MARKER >&2; exit 7", false, true},
		{"timeout", "seq 1 30000; printf TAIL_MARKER; sleep 10", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, err := sandbox.NewSession(&sandbox.Profile{FullAccess: true}, sandbox.ApprovalNever, sandbox.DenyApprover{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tool := &BashTool{workDir: t.TempDir(), readOnly: true, session: session}
			ctx := context.Background()
			if test.timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			args, _ := json.Marshal(bashArgs{Command: test.command})
			started := time.Now()
			output, err := tool.Execute(ctx, args)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output, "TAIL_MARKER") || !strings.Contains(output, "终端输出过长") || len(output) > 8500 {
				t.Fatalf("tail lost or output unbounded: bytes=%d, tail=%v, truncated=%v", len(output), strings.Contains(output, "TAIL_MARKER"), strings.Contains(output, "终端输出过长"))
			}
			if test.failure && !strings.HasPrefix(output, "执行报错:") {
				t.Fatal("command failure lost ordinary result semantics")
			}
			if test.timeout && (!strings.Contains(output, "命令执行超时") || time.Since(started) > 5*time.Second) {
				t.Fatal("timeout lost diagnostics or failed to stop descendants promptly")
			}
		})
	}
}
