package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type TaskStatus string

const (
	TaskRunning TaskStatus = "running"
	TaskExited  TaskStatus = "exited"
	TaskStopped TaskStatus = "stopped"
)

var (
	ErrSessionNotFound  = errors.New("会话不存在")
	ErrSessionForbidden = errors.New("无权访问该会话")
	ErrTaskLimit        = errors.New("后台会话数量已达到上限")
	ErrTaskStart        = errors.New("进程启动失败")
)

type TaskSnapshot struct {
	SessionID       string     `json:"session_id"`
	OwnerID         string     `json:"-"`
	WorkDir         string     `json:"-"`
	Command         string     `json:"command,omitempty"`
	Status          TaskStatus `json:"status"`
	PID             int        `json:"pid,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	Signal          string     `json:"signal,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Output          string     `json:"output,omitempty"`
	NextCursor      int64      `json:"next_cursor,omitempty"`
	OutputTruncated bool       `json:"output_truncated,omitempty"`
}

type taskSession struct {
	mu            sync.Mutex
	sessionID     string
	ownerID       string
	workDir       string
	command       string
	cmd           *exec.Cmd
	status        TaskStatus
	exitCode      *int
	signal        string
	startedAt     time.Time
	finishedAt    *time.Time
	log           *taskLog
	done          chan struct{}
	outputNotify  chan struct{}
	serverCursor  int64
	stopRequested bool
}

type taskOutputWriter struct {
	log    *taskLog
	notify chan<- struct{}
}

func (w taskOutputWriter) Write(p []byte) (int, error) {
	n, err := w.log.Write(p)
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return n, err
}

type TaskManager struct {
	mu         sync.Mutex
	sessions   map[string]*taskSession
	maxActive  int
	maxRecords int
	shutdown   bool
	logLimit   int
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		sessions:   make(map[string]*taskSession),
		maxActive:  16,
		maxRecords: 128,
		logLimit:   defaultTaskLogLimit,
	}
}

func (m *TaskManager) Start(ctx context.Context, ownerID, workDir, command string) (string, error) {
	if command == "" {
		return "", errors.New("命令不能为空")
	}
	ownerID = normalizeOwner(ownerID)
	workDir, err := normalizeWorkDir(workDir)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return "", errors.New("TaskManager 已关闭")
	}
	active := 0
	for _, session := range m.sessions {
		session.mu.Lock()
		if session.status == TaskRunning {
			active++
		}
		session.mu.Unlock()
	}
	if active >= m.maxActive {
		m.mu.Unlock()
		return "", ErrTaskLimit
	}

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = workDir
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	logBuffer := newTaskLog(m.logLimit)
	outputNotify := make(chan struct{}, 1)
	cmd.Stdout = taskOutputWriter{log: logBuffer, notify: outputNotify}
	cmd.Stderr = taskOutputWriter{log: logBuffer, notify: outputNotify}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("%w: %v", ErrTaskStart, err)
	}

	id, err := newSessionID()
	if err != nil {
		terminateProcess(cmd, nil)
		m.mu.Unlock()
		return "", fmt.Errorf("生成会话 ID 失败: %w", err)
	}
	session := &taskSession{
		sessionID:    id,
		ownerID:      ownerID,
		workDir:      workDir,
		command:      command,
		cmd:          cmd,
		status:       TaskRunning,
		startedAt:    time.Now(),
		log:          logBuffer,
		done:         make(chan struct{}),
		outputNotify: outputNotify,
	}
	m.pruneFinishedLocked()
	m.sessions[id] = session
	m.mu.Unlock()

	go m.wait(session)
	go m.watchContext(ctx, session)
	return id, nil
}

func (m *TaskManager) pruneFinishedLocked() {
	if len(m.sessions) < m.maxRecords {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, session := range m.sessions {
		session.mu.Lock()
		finished := session.finishedAt
		status := session.status
		session.mu.Unlock()
		if status != TaskRunning && finished != nil && (oldestID == "" || finished.Before(oldest)) {
			oldestID, oldest = id, *finished
		}
	}
	if oldestID != "" {
		delete(m.sessions, oldestID)
	}
}

func (m *TaskManager) wait(session *taskSession) {
	err := session.cmd.Wait()
	now := time.Now()
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.status == TaskRunning {
		session.status = TaskExited
	}
	if session.status == TaskStopped && session.signal == "" {
		session.signal = signalFromWaitError(err)
	}
	if session.exitCode == nil && session.cmd.ProcessState != nil {
		code := session.cmd.ProcessState.ExitCode()
		session.exitCode = &code
	}
	session.finishedAt = &now
	close(session.done)
}

func (m *TaskManager) watchContext(ctx context.Context, session *taskSession) {
	if ctx == nil {
		return
	}
	select {
	case <-ctx.Done():
		_ = m.Stop(context.Background(), session.sessionID, "", "")
	case <-session.done:
	}
}

func (m *TaskManager) get(ownerID, workDir, id string) (*taskSession, error) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, ErrSessionNotFound
	}
	ownerID = normalizeOwner(ownerID)
	workDir, err := normalizeWorkDir(workDir)
	if err != nil {
		return nil, err
	}
	if session.ownerID != ownerID || session.workDir != workDir {
		return nil, ErrSessionForbidden
	}
	return session, nil
}

func (m *TaskManager) WaitUntil(ctx context.Context, ownerID, workDir, id string, wait time.Duration, maxOutput int) (TaskSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := m.get(ownerID, workDir, id)
	if err != nil {
		return TaskSnapshot{}, err
	}
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-session.done:
	case <-timer.C:
	case <-ctx.Done():
		_ = m.Stop(context.Background(), id, ownerID, workDir)
		return TaskSnapshot{}, ctx.Err()
	}
	return m.snapshot(session, maxOutput, true), nil
}

func (m *TaskManager) Poll(ctx context.Context, ownerID, workDir, id string, wait time.Duration, cursor *int64, maxOutput int) (TaskSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := m.get(ownerID, workDir, id)
	if err != nil {
		return TaskSnapshot{}, err
	}
	if wait < 0 {
		wait = 0
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		snapshot := m.snapshotAt(session, maxOutput, cursor, cursor != nil)
		if snapshot.Output != "" || snapshot.Status != TaskRunning || wait == 0 {
			return snapshot, nil
		}
		select {
		case <-session.done:
		case <-session.outputNotify:
		case <-deadline.C:
			return m.snapshotAt(session, maxOutput, cursor, cursor != nil), nil
		case <-ctx.Done():
			return TaskSnapshot{}, ctx.Err()
		}
	}
}

func (m *TaskManager) snapshot(session *taskSession, maxOutput int, consume bool) TaskSnapshot {
	return m.snapshotAt(session, maxOutput, nil, !consume)
}

func (m *TaskManager) snapshotAt(session *taskSession, maxOutput int, cursor *int64, explicit bool) TaskSnapshot {
	session.mu.Lock()
	status := session.status
	snapshot := TaskSnapshot{
		SessionID:  session.sessionID,
		OwnerID:    session.ownerID,
		WorkDir:    session.workDir,
		Command:    session.command,
		Status:     status,
		PID:        session.cmd.Process.Pid,
		ExitCode:   session.exitCode,
		Signal:     session.signal,
		StartedAt:  session.startedAt,
		FinishedAt: session.finishedAt,
	}
	readCursor := session.serverCursor
	if cursor != nil {
		readCursor = *cursor
	}
	output, next, truncated := session.log.read(readCursor, maxOutput)
	snapshot.Output, snapshot.NextCursor, snapshot.OutputTruncated = output, next, truncated
	if !explicit && cursor == nil {
		session.serverCursor = next
	}
	session.mu.Unlock()
	return snapshot
}

func (m *TaskManager) List(ownerID, workDir string) ([]TaskSnapshot, error) {
	ownerID = normalizeOwner(ownerID)
	workDir, err := normalizeWorkDir(workDir)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]TaskSnapshot, 0)
	for _, session := range m.sessions {
		if session.ownerID != ownerID || session.workDir != workDir {
			continue
		}
		session.mu.Lock()
		result = append(result, TaskSnapshot{
			SessionID:  session.sessionID,
			OwnerID:    session.ownerID,
			WorkDir:    session.workDir,
			Command:    session.command,
			Status:     session.status,
			PID:        session.cmd.Process.Pid,
			ExitCode:   session.exitCode,
			Signal:     session.signal,
			StartedAt:  session.startedAt,
			FinishedAt: session.finishedAt,
		})
		session.mu.Unlock()
	}
	return result, nil
}

func (m *TaskManager) Stop(ctx context.Context, id, ownerID, workDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	session, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}
	if ownerID != "" || workDir != "" {
		var err error
		session, err = m.get(ownerID, workDir, id)
		if err != nil {
			return err
		}
	}
	session.mu.Lock()
	if session.status != TaskRunning {
		session.mu.Unlock()
		return nil
	}
	session.status = TaskStopped
	session.stopRequested = true
	session.signal = "SIGTERM"
	cmd := session.cmd
	session.mu.Unlock()
	terminateProcess(cmd, session.done)
	return nil
}

func (m *TaskManager) Shutdown() {
	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return
	}
	m.shutdown = true
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(context.Background(), id, "", "")
	}
}

func normalizeOwner(ownerID string) string {
	if ownerID == "" {
		return "default"
	}
	return ownerID
}

func normalizeWorkDir(workDir string) (string, error) {
	if workDir == "" {
		return "", errors.New("工作区不能为空")
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("解析工作区失败: %w", err)
	}
	return filepath.Clean(abs), nil
}

func newSessionID() (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "bash-session-" + hex.EncodeToString(bytes), nil
}

func signalFromWaitError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
