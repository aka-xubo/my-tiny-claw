//go:build !windows

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitForTaskFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("command did not become ready: %s", path)
	return ""
}

func assertTaskProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		// On Linux a dead orphan may remain a zombie until PID 1 reaps it.
		out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("process %d survived cleanup", pid)
}

func TestStopAndShutdownCleanOrphanedGroups(t *testing.T) {
	for _, action := range []string{"stop", "shutdown", "evict", "inherited-pipe"} {
		t.Run(action, func(t *testing.T) {
			m := NewTaskManager()
			defer m.Shutdown()
			m.maxRecords = 1
			dir := t.TempDir()
			command := "sleep 100 >/dev/null 2>&1 & echo $! > child.pid; echo hi"
			if action == "inherited-pipe" {
				command = "sleep 100 & echo $! > child.pid; echo hi"
			}
			id, err := m.Start(context.Background(), "o", dir, command)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(waitForTaskFile(t, filepath.Join(dir, "child.pid")))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			s, err := m.WaitUntil(context.Background(), "o", dir, id, 5*time.Second, 8000)
			if err != nil || s.Status != TaskExited {
				t.Fatalf("shell did not exit: %+v %v", s, err)
			}
			switch action {
			case "stop":
				if err := m.Stop(context.Background(), id, "o", dir); err != nil {
					t.Fatal(err)
				}
			case "evict":
				if _, err := m.Start(context.Background(), "o", dir, "true"); err != nil {
					t.Fatal(err)
				}
			default:
				m.Shutdown()
			}
			assertTaskProcessGone(t, pid)
		})
	}
}

func TestStopKillsDescendantsAfterLeaderExits(t *testing.T) {
	m := NewTaskManager()
	defer m.Shutdown()
	dir := t.TempDir()
	id, err := m.Start(context.Background(), "o", dir,
		`bash -c 'trap "" TERM; echo $$ > child.pid; while :; do sleep 1; done' & wait`)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(waitForTaskFile(t, filepath.Join(dir, "child.pid")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := m.Stop(context.Background(), id, "o", dir); err != nil {
		t.Fatal(err)
	}
	assertTaskProcessGone(t, pid)
}

func TestShutdownUsesOneGracePeriodAndRecordsActualSignal(t *testing.T) {
	m := NewTaskManager()
	defer m.Shutdown()
	dir := t.TempDir()
	for i := 0; i < 4; i++ {
		file := "ready" + strconv.Itoa(i)
		_, err := m.Start(context.Background(), "o", dir,
			"trap '' TERM; printf ready > "+file+"; while :; do sleep 1; done")
		if err != nil {
			t.Fatal(err)
		}
		waitForTaskFile(t, filepath.Join(dir, file))
	}
	start := time.Now()
	m.Shutdown()
	elapsed := time.Since(start)
	if elapsed < 1900*time.Millisecond || elapsed > 3500*time.Millisecond {
		t.Fatalf("expected one 2-second grace period, took %s", elapsed)
	}
	for _, session := range m.sessions {
		if session.status != TaskStopped || session.signal != syscall.SIGKILL.String() {
			t.Fatalf("wrong actual signal/status: %s %s", session.status, session.signal)
		}
		assertTaskProcessGone(t, session.cmd.Process.Pid)
	}
}

func TestSignalFieldReflectsNaturalSignalAndOrdinaryExit(t *testing.T) {
	for _, command := range []string{"exit 7", "kill -TERM $$"} {
		m := NewTaskManager()
		t.Cleanup(m.Shutdown)
		dir := t.TempDir()
		id, err := m.Start(context.Background(), "o", dir, command)
		if err != nil {
			t.Fatal(err)
		}
		s, err := m.WaitUntil(context.Background(), "o", dir, id, time.Second, 8000)
		if err != nil || s.Status != TaskExited || s.ExitCode == nil {
			t.Fatalf("missing exit state: %+v %v", s, err)
		}
		if command == "exit 7" && (s.Signal != "" || *s.ExitCode != 7) {
			t.Fatalf("ordinary error is not a signal: %+v", s)
		}
		if command != "exit 7" && s.Signal != syscall.SIGTERM.String() {
			t.Fatalf("missing natural termination signal: %+v", s)
		}
	}
}
