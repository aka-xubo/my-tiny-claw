package tools

import (
	"context"
	"encoding/json"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestManagedSandboxSessionRetainsLaunchPermissions(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS")
	}
	requireReadOnlySandbox(t)
	ws := t.TempDir()
	profiles, err := sandbox.BuiltinProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	session, err := sandbox.NewSession(profiles[":workspace"], sandbox.ApprovalNever, nil, []string{ws})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	manager := NewTaskManager()
	defer manager.Shutdown()
	tool := NewManagedBashToolWithSandbox(ws, manager, session)
	ctx, cancel := context.WithCancel(context.Background())
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"sleep 1; printf retained > retained.txt","yield_time_ms":250}`))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data TaskSnapshot `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatal(err)
	}
	snapshot := response.Data
	if snapshot.Status != TaskRunning || snapshot.Profile != ":workspace" || snapshot.Escalated {
		t.Fatalf("launch metadata: %s", output)
	}
	cancel()
	if err := session.SetProfile(profiles[":read-only"]); err != nil {
		t.Fatal(err)
	}
	snapshot, err = manager.WaitUntil(context.Background(), "default", ws, snapshot.SessionID, 3*time.Second, 8000)
	if err != nil || snapshot.ExitCode == nil || *snapshot.ExitCode != 0 {
		t.Fatalf("old session changed permissions: %+v %v", snapshot, err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "retained.txt"))
	if err != nil || string(data) != "retained" {
		t.Fatalf("old process canceled or restricted: %s %v", data, err)
	}
	output, err = tool.Execute(context.Background(), json.RawMessage(`{"command":"printf x > new.txt"}`))
	if err != nil || !strings.Contains(output, "Operation not permitted") {
		t.Fatalf("new profile not enforced: %s %v", output, err)
	}
}
func TestBackgroundManagementRejectsPermissionParameters(t *testing.T) {
	session, _ := sandbox.NewSession(&sandbox.Profile{FullAccess: true}, sandbox.ApprovalNever, nil, nil)
	defer session.Close()
	manager := NewTaskManager()
	defer manager.Shutdown()
	tool := NewManagedBashToolWithSandbox(t.TempDir(), manager, session)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"list","sandbox_permissions":"use_default"}`)); err == nil {
		t.Fatal("management accepted privilege parameter")
	}
}
