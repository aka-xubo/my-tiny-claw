package tools

import (
	"encoding/json"
	"testing"

	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
)

func TestSandboxToolArgsAreFlattenedInJSON(t *testing.T) {
	var args bashArgs
	if err := json.Unmarshal([]byte(`{"command":"git status","sandbox_permissions":"require_escalated","justification":"test"}`), &args); err != nil {
		t.Fatal(err)
	}
	if args.SandboxPermissions != sandbox.RequireEscalated || args.Justification != "test" {
		t.Fatalf("sandbox args were not decoded: %+v", args.sandboxToolArgs)
	}
}
