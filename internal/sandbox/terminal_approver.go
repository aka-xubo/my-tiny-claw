package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type pendingApproval struct {
	request            ApprovalRequest
	result             chan ApprovalResult
	foreground, reason bool
}

// TerminalApprover is the sole stdin reader after startup trust confirmation.
// REPL and approval prompts consume routed events, never read stdin themselves.
type TerminalApprover struct {
	mu             sync.Mutex
	reader         *bufio.Reader
	out            io.Writer
	inTurn         bool
	turnGeneration int
	nextID         int
	pending        map[int]*pendingApproval
	active         int
	once           sync.Once
	lines          chan string
	eof            chan struct{}
	ctx            context.Context
	cancel         context.CancelFunc
	gate           chan struct{}
	pendingTimeout time.Duration
}

func NewTerminalApprover(reader io.Reader, out io.Writer) *TerminalApprover {
	ctx, cancel := context.WithCancel(context.Background())
	return &TerminalApprover{reader: bufio.NewReader(reader), out: out, pending: map[int]*pendingApproval{}, lines: make(chan string, 1), eof: make(chan struct{}), ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1), pendingTimeout: 60 * time.Second}
}
func (a *TerminalApprover) start() {
	a.once.Do(func() {
		go func() {
			defer close(a.eof)
			for {
				line, err := a.reader.ReadString('\n')
				if len(line) > 0 {
					a.RouteLine(strings.TrimRight(line, "\r\n"))
				}
				if err != nil {
					return
				}
			}
		}()
	})
}
func (a *TerminalApprover) Close() { a.cancel() }
func (a *TerminalApprover) BeginTurn() {
	a.mu.Lock()
	a.inTurn = true
	a.turnGeneration++
	a.mu.Unlock()
}
func (a *TerminalApprover) EndTurn() { a.mu.Lock(); a.inTurn = false; a.mu.Unlock() }

func (a *TerminalApprover) InTurn() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.inTurn }

func (a *TerminalApprover) CancelTurnApprovals() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turnGeneration++
	for id, p := range a.pending {
		if p.foreground {
			delete(a.pending, id)
			p.result <- ApprovalResult{Decision: Deny, Feedback: "当前回合已取消"}
			if a.active == id {
				a.active = 0
			}
		}
	}
}
func (a *TerminalApprover) NextLine(ctx context.Context) (string, error) {
	a.start()
	select {
	case line := <-a.lines:
		return line, nil
	default:
	}
	select {
	case line := <-a.lines:
		return line, nil
	case <-a.eof:
		select {
		case line := <-a.lines:
			return line, nil
		default:
			return "", io.EOF
		}
	case <-ctx.Done():
		return "", ctx.Err()
	case <-a.ctx.Done():
		return "", a.ctx.Err()
	}
}
func (a *TerminalApprover) Pending() map[int]ApprovalRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := map[int]ApprovalRequest{}
	for id, p := range a.pending {
		r[id] = p.request
	}
	return r
}
func (a *TerminalApprover) Submit(id int, decision Decision, feedback string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[id]
	if !ok {
		return fmt.Errorf("审批编号不存在: %d", id)
	}
	if decision != AllowOnce && decision != AllowSession && decision != Deny {
		return errors.New("审批决定非法")
	}
	delete(a.pending, id)
	if a.active == id {
		a.active = 0
	}
	p.result <- ApprovalResult{Decision: decision, Feedback: feedback}
	return nil
}
func (a *TerminalApprover) RouteLine(line string) {
	a.mu.Lock()
	// Numbered answers are explicit even if a new agent turn is in progress.
	if line == "/approve" {
		ids := make([]int, 0, len(a.pending))
		for id := range a.pending {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		if len(ids) == 0 {
			fmt.Fprintln(a.out, "没有待处理的审批。")
		}
		for _, id := range ids {
			p := a.pending[id]
			fmt.Fprintf(a.out, "审批 #%d：%s %s:%d %s\n", id, approvalKindName(p.request.Kind), p.request.Host, p.request.Port, p.request.Command)
		}
		a.mu.Unlock()
		return
	}
	if strings.HasPrefix(line, "/approve ") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			fmt.Fprintln(a.out, "用法：/approve <编号> y|a|n|r [拒绝原因]")
			a.mu.Unlock()
			return
		}
		id, err := strconv.Atoi(fields[1])
		decision, ok := parseDecision(fields[2])
		if err != nil || !ok {
			fmt.Fprintln(a.out, "审批编号或选项无效。")
			a.mu.Unlock()
			return
		}
		feedback := strings.Join(fields[3:], " ")
		if fields[2] == "r" && feedback == "" {
			fmt.Fprintln(a.out, "请使用 /approve <编号> r <拒绝原因>")
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
		if err := a.Submit(id, decision, feedback); err != nil {
			a.mu.Lock()
			fmt.Fprintln(a.out, err)
			a.mu.Unlock()
		}
		return
	}
	if a.inTurn {
		p := a.pending[a.active]
		if p == nil {
			fmt.Fprintln(a.out, "agent 运行中，请等待或按 Ctrl-C 取消")
			a.mu.Unlock()
			return
		}
		id := a.active
		if p.reason {
			a.mu.Unlock()
			_ = a.Submit(id, Deny, line)
			return
		}
		answer := strings.TrimSpace(line)
		if answer == "r" {
			p.reason = true
			fmt.Fprint(a.out, "拒绝原因 > ")
			a.mu.Unlock()
			return
		}
		decision, ok := parseDecision(answer)
		if !ok {
			fmt.Fprint(a.out, "请输入 y、a、n 或 r > ")
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
		_ = a.Submit(id, decision, "")
		return
	}
	a.mu.Unlock()
	select {
	case a.lines <- line:
	case <-a.ctx.Done():
	}
}
func parseDecision(answer string) (Decision, bool) {
	switch answer {
	case "y":
		return AllowOnce, true
	case "a":
		return AllowSession, true
	case "n", "r":
		return Deny, true
	default:
		return Deny, false
	}
}
func (a *TerminalApprover) Approve(ctx context.Context, request ApprovalRequest) (ApprovalResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ApprovalResult{Decision: Deny}, err
	}
	a.mu.Lock()
	foreground := a.inTurn
	generation := a.turnGeneration
	a.mu.Unlock()
	if foreground {
		select {
		case a.gate <- struct{}{}:
			defer func() { <-a.gate }()
		case <-ctx.Done():
			return ApprovalResult{Decision: Deny}, ctx.Err()
		case <-a.ctx.Done():
			return ApprovalResult{Decision: Deny}, a.ctx.Err()
		}
	}
	a.mu.Lock()
	if foreground && generation != a.turnGeneration {
		a.mu.Unlock()
		return ApprovalResult{Decision: Deny, Feedback: "当前回合已取消"}, nil
	}
	foreground = a.inTurn
	a.nextID++
	id := a.nextID
	p := &pendingApproval{request: request, result: make(chan ApprovalResult, 1), foreground: foreground}
	a.pending[id] = p
	if foreground {
		a.active = id
		a.printPrompt(request)
	} else {
		fmt.Fprintf(a.out, "\n[审批 #%d] %s:%d %s；输入 /approve %d y|a|n|r，60 秒后自动拒绝\nclaw> ", id, request.Host, request.Port, request.Command, id)
		for _, source := range request.Sources {
			fmt.Fprintf(a.out, "规则组候选来源：%s\n", source)
		}
	}
	timeout := a.pendingTimeout
	a.mu.Unlock()
	// Input begins only after the approval is registered, so piped answers
	// cannot race ahead of the first foreground prompt.
	a.start()
	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if !foreground {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		if a.active == id {
			a.active = 0
		}
		a.mu.Unlock()
	}()
	select {
	case r := <-p.result:
		return r, nil
	default:
	}
	select {
	case r := <-p.result:
		if ctx.Err() != nil {
			return ApprovalResult{Decision: Deny}, ctx.Err()
		}
		return r, nil
	case <-ctx.Done():
		return ApprovalResult{Decision: Deny, Feedback: "审批已取消"}, ctx.Err()
	case <-a.ctx.Done():
		return ApprovalResult{Decision: Deny}, a.ctx.Err()
	case <-timeoutCh:
		return ApprovalResult{Decision: Deny, Feedback: "审批超时"}, nil
	case <-a.eof:
		select {
		case r := <-p.result:
			return r, nil
		default:
			return ApprovalResult{Decision: Deny, Feedback: "输入已关闭"}, io.EOF
		}
	}
}
func (a *TerminalApprover) printPrompt(r ApprovalRequest) {
	fmt.Fprintf(a.out, "\n┌ 需要审批 · %s\n│ 工具: %s\n│ 命令: %s\n│ 目录: %s\n│ 理由: %s\n", approvalKindName(r.Kind), r.Tool, r.Command, r.Dir, r.Justification)
	if r.Kind == ApprovalNetwork {
		fmt.Fprintf(a.out, "│ 网络: %s:%d\n", r.Host, r.Port)
		for _, source := range r.Sources {
			fmt.Fprintf(a.out, "│ 规则组候选来源: %s\n", source)
		}
	}
	if r.Additional != nil {
		paths := make([]string, 0, len(r.Additional.Filesystem))
		for path, access := range r.Additional.Filesystem {
			paths = append(paths, path+"="+accessName(access))
		}
		sort.Strings(paths)
		for _, path := range paths {
			fmt.Fprintf(a.out, "│ 额外权限: %s\n", path)
		}
		if r.Additional.Network != NetworkDisabled {
			fmt.Fprintf(a.out, "│ 网络: %s\n", networkModeName(r.Additional.Network))
		}
	}
	fmt.Fprint(a.out, "└ [y] 允许一次  [a] 本会话允许  [n] 拒绝  [r] 拒绝并说明原因 > ")
}
func accessName(a Access) string {
	if a == AccessWrite {
		return "write"
	}
	return "read"
}
func approvalKindName(k ApprovalKind) string {
	switch k {
	case ApprovalCommand:
		return "bash"
	case ApprovalFileWrite:
		return "文件写入"
	case ApprovalEscalation:
		return "完全提权"
	case ApprovalAdditionalPermissions:
		return "额外权限"
	case ApprovalNetwork:
		return "网络访问"
	default:
		return "权限"
	}
}
