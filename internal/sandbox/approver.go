package sandbox

import "context"

type ApprovalKind int

const (
	ApprovalCommand ApprovalKind = iota
	ApprovalFileWrite
	ApprovalEscalation
	ApprovalAdditionalPermissions
	ApprovalNetwork
)

type ApprovalRequest struct {
	Kind          ApprovalKind
	Tool          string
	Command       string
	Dir           string
	Justification string
	Additional    *AdditionalPermissions
	Host          string
	Port          int
	Sources       []string // Candidate commands using the shared rule group.
}

type Decision int

const (
	AllowOnce Decision = iota
	AllowSession
	Deny
)

type ApprovalResult struct {
	Decision Decision
	Feedback string
}

type Approver interface {
	Approve(context.Context, ApprovalRequest) (ApprovalResult, error)
}

type DenyApprover struct{}

func (DenyApprover) Approve(_ context.Context, _ ApprovalRequest) (ApprovalResult, error) {
	return ApprovalResult{Decision: Deny, Feedback: "非交互模式无法审批"}, nil
}
