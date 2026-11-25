package sandbox

import (
	"context"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox/proxy"
	"testing"
)

type recordingApprover struct {
	result ApprovalResult
	calls  []ApprovalRequest
}

func (a *recordingApprover) Approve(_ context.Context, request ApprovalRequest) (ApprovalResult, error) {
	a.calls = append(a.calls, request)
	return a.result, nil
}

func testSession(t *testing.T, policy ApprovalPolicy, result ApprovalResult) (*Session, *recordingApprover) {
	t.Helper()
	profile := &Profile{
		Name:  ":workspace",
		Files: []FileRule{{Path: "/workspace", Access: AccessWrite}},
	}
	approver := &recordingApprover{result: result}
	session, err := NewSession(profile, policy, approver, []string{"/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	return session, approver
}

func TestAuthorizeOnRequestDefaultDoesNotAsk(t *testing.T) {
	session, approver := testSession(t, ApprovalOnRequest, ApprovalResult{Decision: Deny})
	profile, err := session.Authorize(context.Background(), CallPermissions{
		Tool:    "bash",
		Command: "git status",
		Dir:     "/workspace",
	})
	if err != nil || profile == nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if len(approver.calls) != 0 {
		t.Fatalf("on-request default unexpectedly asked for approval")
	}
}

func TestAuthorizeUntrustedBashAsksAndCachesSessionApproval(t *testing.T) {
	session, approver := testSession(t, ApprovalUntrusted, ApprovalResult{Decision: AllowSession})
	call := CallPermissions{Tool: "bash", Command: "pwd", Dir: "/workspace"}
	if _, err := session.Authorize(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Authorize(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if len(approver.calls) != 1 {
		t.Fatalf("approval calls = %d, want 1", len(approver.calls))
	}
}

func TestAuthorizeAdditionalPermissionsUsesSubsetCache(t *testing.T) {
	session, approver := testSession(t, ApprovalOnRequest, ApprovalResult{Decision: AllowSession})
	call := CallPermissions{
		Tool:               "bash",
		Command:            "git commit -m x",
		Dir:                "/workspace",
		SandboxPermissions: WithAdditionalPermissions,
		AdditionalPermissions: &AdditionalPermissions{Filesystem: map[string]Access{
			"/workspace/.git": AccessWrite,
			"/workspace/tmp":  AccessWrite,
		}},
	}
	if _, err := session.Authorize(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	call.AdditionalPermissions = &AdditionalPermissions{Filesystem: map[string]Access{
		"/workspace/.git": AccessWrite,
	}}
	if _, err := session.Authorize(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if len(approver.calls) != 1 {
		t.Fatalf("approval calls = %d, want 1", len(approver.calls))
	}
}

func TestAuthorizeNeverRejectsEscalation(t *testing.T) {
	session, approver := testSession(t, ApprovalNever, ApprovalResult{Decision: AllowSession})
	_, err := session.Authorize(context.Background(), CallPermissions{
		Tool:               "bash",
		Command:            "git commit -m x",
		Dir:                "/workspace",
		SandboxPermissions: RequireEscalated,
	})
	if err == nil {
		t.Fatal("expected escalation to be rejected")
	}
	if len(approver.calls) != 0 {
		t.Fatal("never policy should not call approver")
	}
}

func TestApprovalPolicyMatrix(t *testing.T) {
	for _, policy := range []ApprovalPolicy{ApprovalUntrusted, ApprovalOnRequest, ApprovalNever} {
		for _, kind := range []string{"bash", "read_file", "write_file", "edit_file", "additional", "escalation", "network"} {
			t.Run(string(policy)+"/"+kind, func(t *testing.T) {
				session, a := testSession(t, policy, ApprovalResult{Decision: Deny})
				defer session.Close()
				needApproval := kind == "additional" || kind == "escalation" || kind == "network" || policy == ApprovalUntrusted && kind != "read_file"
				denied := needApproval
				call := CallPermissions{Tool: kind, Command: "test", Dir: "/workspace", Justification: "integration test"}
				if kind == "additional" {
					call.SandboxPermissions = WithAdditionalPermissions
					call.AdditionalPermissions = &AdditionalPermissions{Filesystem: map[string]Access{"/workspace/.git": AccessWrite}}
				}
				if kind == "escalation" {
					call.SandboxPermissions = RequireEscalated
				}
				if kind == "network" {
					decision, err := session.approveNetwork(context.Background(), "example.com", 443, policy)
					if err != nil || decision != proxy.Deny {
						t.Fatalf("network result=%v err=%v", decision, err)
					}
				} else {
					_, err := session.Authorize(context.Background(), call)
					if (err != nil) != denied {
						t.Fatalf("denied=%v err=%v", denied, err)
					}
				}
				want := 0
				if needApproval && policy != ApprovalNever {
					want = 1
				}
				if len(a.calls) != want {
					t.Fatalf("approval calls=%d want=%d", len(a.calls), want)
				}
			})
		}
	}
}
func TestNeverOverridesExistingSessionApprovals(t *testing.T) {
	for _, kind := range []string{"additional", "escalation", "network"} {
		t.Run(kind, func(t *testing.T) {
			session, _ := testSession(t, ApprovalOnRequest, ApprovalResult{Decision: AllowSession})
			defer session.Close()
			call := CallPermissions{Tool: "bash", Command: "test", Dir: "/workspace", Justification: "test", SandboxPermissions: RequireEscalated}
			if kind == "additional" {
				call.SandboxPermissions = WithAdditionalPermissions
				call.AdditionalPermissions = &AdditionalPermissions{Filesystem: map[string]Access{"/workspace/.git": AccessWrite}}
			}
			if kind == "network" {
				_, _ = session.approveNetwork(context.Background(), "example.com", 443, ApprovalOnRequest)
			} else {
				if _, err := session.Authorize(context.Background(), call); err != nil {
					t.Fatal(err)
				}
			}
			_ = session.SetPolicy(ApprovalNever)
			if kind == "network" {
				decision, _ := session.approveNetwork(context.Background(), "example.com", 443, session.Policy())
				if decision != proxy.Deny {
					t.Fatal("cache bypassed never")
				}
			} else {
				if _, err := session.Authorize(context.Background(), call); err == nil {
					t.Fatal("cache bypassed never")
				}
			}
		})
	}
}
func TestApprovalCacheScopeAndCancellation(t *testing.T) {
	session, a := testSession(t, ApprovalUntrusted, ApprovalResult{Decision: AllowSession})
	defer session.Close()
	call := CallPermissions{Tool: "bash", Command: "./deploy.sh", Dir: "/workspace/a"}
	for _, dir := range []string{"/workspace/a", "/workspace/a", "/workspace/b"} {
		call.Dir = dir
		if _, err := session.Authorize(context.Background(), call); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.calls) != 2 {
		t.Fatal("working directory not scoped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.Authorize(ctx, call); err == nil {
		t.Fatal("canceled call permitted")
	}
}
func TestApprovalRelativePathsAndInvalidEscalation(t *testing.T) {
	session, a := testSession(t, ApprovalOnRequest, ApprovalResult{Decision: AllowOnce})
	defer session.Close()
	call := CallPermissions{Tool: "bash", Dir: "/workspace", SandboxPermissions: WithAdditionalPermissions, AdditionalPermissions: &AdditionalPermissions{Filesystem: map[string]Access{".git": AccessWrite}}}
	p, err := session.Authorize(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if a.calls[0].Additional.Filesystem["/workspace/.git"] != AccessWrite {
		t.Fatal("relative path shown incorrectly")
	}
	if !p.Files[len(p.Files)-1].Approved {
		t.Fatal("approval missing from profile")
	}
	call.SandboxPermissions = RequireEscalated
	call.Justification = ""
	if _, err := session.Authorize(context.Background(), call); err == nil {
		t.Fatal("missing justification accepted")
	}
}

func TestAdditionalSubsetIncludesDescendantsButNotSiblings(t *testing.T) {
	cached := AdditionalPermissions{Filesystem: map[string]Access{"/workspace/.git": AccessWrite}, Network: NetworkFull}
	for _, test := range []struct {
		path string
		want bool
	}{{"/workspace/.git/hooks", true}, {"/workspace/.git-other", false}} {
		if got := additionalSubset(AdditionalPermissions{Filesystem: map[string]Access{test.path: AccessRead}, Network: NetworkProxy}, cached); got != test.want {
			t.Fatalf("%s subset=%v", test.path, got)
		}
	}
}
func TestFullAccessStillHonorsUntrustedOrdinaryApproval(t *testing.T) {
	a := &recordingApprover{result: ApprovalResult{Decision: Deny}}
	s, err := NewSession(&Profile{FullAccess: true}, ApprovalUntrusted, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Authorize(context.Background(), CallPermissions{Tool: "bash", Command: "pwd"}); err == nil {
		t.Fatal("full access bypassed untrusted approval")
	}
}
