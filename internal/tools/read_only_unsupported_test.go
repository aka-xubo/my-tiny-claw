//go:build !darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
	"testing"
)

func TestReadOnlyBashFailsClosedOnUnsupportedPlatform(t *testing.T) {
	tool := NewReadOnlyBashTool(t.TempDir())
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo must-not-run"}`))
	if !errors.Is(err, sandbox.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported: %v", err)
	}
}
