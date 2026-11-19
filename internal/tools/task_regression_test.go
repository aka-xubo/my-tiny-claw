package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFinishedOutputPrioritizesTailAndPreservesReplay(t *testing.T) {
	m := NewTaskManager()
	defer m.Shutdown()
	dir := t.TempDir()
	id, err := m.Start(context.Background(), "o", dir, "printf HEAD_MARKER; seq 1 30000; printf TAIL_MARKER")
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.WaitUntil(context.Background(), "o", dir, id, 5*time.Second, 8000)
	if err != nil || s.Status != TaskExited || !strings.HasSuffix(s.Output, "TAIL_MARKER") || !s.OutputTruncated || len(s.Output) != 8000 {
		t.Fatalf("missing final diagnostics: %+v, %v", s, err)
	}
	session, _ := m.get("o", dir, id)
	if s.NextCursor != session.log.end() || session.serverCursor != s.NextCursor {
		t.Fatalf("tail must advance to end: %+v", s)
	}
	// An implicit poll of an exited session replays final diagnostics even after
	// the initial response consumed the server cursor.
	poll, err := m.Poll(context.Background(), "o", dir, id, 0, nil, 100)
	if err != nil || !strings.HasSuffix(poll.Output, "TAIL_MARKER") || !poll.OutputTruncated || len(poll.Output) != 100 {
		t.Fatalf("finished poll must return tail: %+v, %v", poll, err)
	}
	cursor := s.NextCursor - 11
	poll, err = m.Poll(context.Background(), "o", dir, id, 0, &cursor, 8000)
	if err != nil || poll.Output != "TAIL_MARKER" || poll.OutputTruncated {
		t.Fatalf("explicit cursor must be honored: %+v, %v", poll, err)
	}
	cursor = s.NextCursor
	poll, err = m.Poll(context.Background(), "o", dir, id, 0, &cursor, 8000)
	if err != nil || poll.Output != "" {
		t.Fatalf("explicit end cursor should be empty: %+v, %v", poll, err)
	}
}

func TestTaskLogTruncation(t *testing.T) {
	for _, size := range []int{5, 20, 100} {
		l := newTaskLog(30)
		l.Write([]byte(strings.Repeat("x", size)))
		_, next, truncated := l.tail(10)
		if truncated != (size > 10) || next != int64(size) {
			t.Fatalf("tail(%d): next=%d truncated=%v", size, next, truncated)
		}
		out, _, truncated := l.read(0, 10)
		if truncated != (size > 10) || len(out) > 10 {
			t.Fatalf("read(%d): len=%d truncated=%v", size, len(out), truncated)
		}
	}
}

func TestYieldedSessionOutlivesCallingContext(t *testing.T) {
	m := NewTaskManager()
	defer m.Shutdown()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, err := m.Start(ctx, "o", dir, "sleep 0.2; printf survived")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitUntil(ctx, "o", dir, id, 10*time.Millisecond, 8000); err != nil {
		t.Fatal(err)
	}
	cancel()
	s, err := m.WaitUntil(context.Background(), "o", dir, id, time.Second, 8000)
	if err != nil || s.Status != TaskExited || s.Output != "survived" || s.Signal != "" {
		t.Fatalf("yielded session still tied to call context: %+v %v", s, err)
	}
}

func TestStartFailureAndCancellationReleaseSlots(t *testing.T) {
	m := NewTaskManager()
	defer m.Shutdown()
	m.maxActive = 1
	dir := t.TempDir()
	if _, err := m.Start(context.Background(), "o", dir+"/missing", "true"); !errors.Is(err, ErrTaskStart) {
		t.Fatalf("expected start failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Start(ctx, "o", dir, "true"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled start: %v", err)
	}
	if _, err := m.Start(context.Background(), "o", dir, "sleep 10"); err != nil {
		t.Fatalf("failed start leaked slot: %v", err)
	}
	if _, err := m.Start(context.Background(), "o", dir, "true"); !errors.Is(err, ErrTaskLimit) {
		t.Fatalf("expected limit: %v", err)
	}
}

func TestConcurrentPollStopListAndShutdown(t *testing.T) {
	m := NewTaskManager()
	defer m.Shutdown()
	dir := t.TempDir()
	id, err := m.Start(context.Background(), "o", dir, "printf ready; sleep 10")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			if _, err := m.Poll(context.Background(), "o", dir, id, time.Millisecond, nil, 8000); err != nil {
				t.Error(err)
			}
			if _, err := m.List("o", dir); err != nil {
				t.Error(err)
			}
			if err := m.Stop(context.Background(), id, "o", dir); err != nil {
				t.Error(err)
			}
		})
	}
	for i := 0; i < 3; i++ {
		wg.Go(m.Shutdown)
	}
	wg.Wait()
	if _, err := m.Start(context.Background(), "o", dir, "true"); err == nil {
		t.Fatal("start succeeded after shutdown")
	}
}

func TestConcurrentStartsAndShutdown(t *testing.T) {
	m := NewTaskManager()
	defer m.Shutdown()
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() { _, _ = m.Start(context.Background(), "o", dir, "sleep 10") })
	}
	wg.Go(m.Shutdown)
	wg.Wait()
	for _, session := range m.sessions {
		select {
		case <-session.done:
		default:
			t.Fatal("shutdown missed a concurrent start")
		}
	}
	if m.pending != 0 {
		t.Fatalf("leaked pending slots: %d", m.pending)
	}
}
