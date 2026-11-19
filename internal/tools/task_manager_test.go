package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTaskManagerWaitWindowKeepsProcessRunningAndPollsOutput(t *testing.T) {
	manager := NewTaskManager()
	defer manager.Shutdown()
	workDir := t.TempDir()
	id, err := manager.Start(context.Background(), "owner-a", workDir, "sleep 0.2; printf ready")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.WaitUntil(context.Background(), "owner-a", workDir, id, 50*time.Millisecond, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != TaskRunning {
		t.Fatalf("等待窗口结束时进程应继续运行: %+v", snapshot)
	}

	snapshot, err = manager.Poll(context.Background(), "owner-a", workDir, id, 1*time.Second, nil, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Output != "ready" {
		t.Fatalf("轮询应得到输出: %+v", snapshot)
	}
	if snapshot.Status == TaskRunning {
		snapshot, err = manager.Poll(context.Background(), "owner-a", workDir, id, 1*time.Second, nil, 8000)
		if err != nil {
			t.Fatal(err)
		}
	}
	if snapshot.Status != TaskExited {
		t.Fatalf("轮询最终应得到退出状态: %+v", snapshot)
	}
}

func TestTaskManagerOwnerAndWorkDirIsolation(t *testing.T) {
	manager := NewTaskManager()
	defer manager.Shutdown()
	workDir := t.TempDir()
	id, err := manager.Start(context.Background(), "owner-a", workDir, "sleep 10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Poll(context.Background(), "owner-b", workDir, id, 0, nil, 8000); err != ErrSessionForbidden {
		t.Fatalf("跨 owner 访问应被拒绝，got %v", err)
	}
	if _, err := manager.List("owner-b", workDir); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), id, "owner-a", workDir); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Poll(context.Background(), "owner-a", workDir, id, 0, nil, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != TaskStopped {
		t.Fatalf("stop 后状态应为 stopped: %+v", snapshot)
	}
}

func TestTaskManagerContextCancellationStopsSession(t *testing.T) {
	manager := NewTaskManager()
	defer manager.Shutdown()
	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	id, err := manager.Start(ctx, "owner-a", workDir, "sleep 10")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := manager.WaitUntil(ctx, "owner-a", workDir, id, time.Second, 8000); err != context.Canceled {
		t.Fatalf("前台取消应返回取消错误: %v", err)
	}
	snapshot, err := manager.Poll(context.Background(), "owner-a", workDir, id, 0, nil, 8000)
	if err != nil || snapshot.Status != TaskStopped {
		t.Fatalf("前台取消应停止会话: %+v, %v", snapshot, err)
	}
}

func TestManagedBashProtocol(t *testing.T) {
	manager := NewTaskManager()
	defer manager.Shutdown()
	workDir := t.TempDir()
	tool := NewManagedBashTool(workDir, manager)
	ctx := WithExecutionContext(context.Background(), "owner-a", workDir)

	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"printf hello","yield_time_ms":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, `"status":"exited"`) || !strings.Contains(output, "hello") {
		t.Fatalf("短命令响应不符合协议: %s", output)
	}

	output, err = tool.Execute(ctx, json.RawMessage(`{"command":"sleep 10","yield_time_ms":250}`))
	if err != nil {
		t.Fatal(err)
	}
	var response bashResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatal(err)
	}
	snapshotBytes, _ := json.Marshal(response.Data)
	var snapshot TaskSnapshot
	if err := json.Unmarshal(snapshotBytes, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.SessionID == "" || snapshot.Status != TaskRunning {
		t.Fatalf("长命令应返回 running 会话: %s", output)
	}

	pollArgs, _ := json.Marshal(map[string]interface{}{"action": "stop", "session_id": snapshot.SessionID})
	if _, err := tool.Execute(ctx, pollArgs); err != nil {
		t.Fatal(err)
	}
}
