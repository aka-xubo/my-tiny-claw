package proxy

import (
	"context"
	"net"
	"testing"
)

func TestNormalizeHostAndMatch(t *testing.T) {
	host, _, err := NormalizeHost("API.Example.COM.")
	if err != nil || host != "api.example.com" {
		t.Fatalf("normalized host = %q, err = %v", host, err)
	}
	rules := Rules{"*.example.com": RuleAllow, "evil.example.com": RuleDeny}
	if got := Match(host, rules); got != Allow {
		t.Fatalf("subdomain match = %v, want allow", got)
	}
	if got := Match("example.com", rules); got != NoMatch {
		t.Fatalf("wildcard should not match apex: %v", got)
	}
	if got := Match("evil.example.com", rules); got != Deny {
		t.Fatalf("deny should win: %v", got)
	}
}

func TestAuthorizeApprovesBeforeDNSAndRejectsPrivateResult(t *testing.T) {
	lookups := 0
	approvals := 0
	server := &Server{
		Rules: Rules{},
		Approve: func(context.Context, string, int) (Decision, error) {
			approvals++
			return Allow, nil
		},
		Resolve: func(context.Context, string) ([]net.IP, error) {
			lookups++
			return []net.IP{net.ParseIP("192.168.1.2")}, nil
		},
	}
	if _, _, err := server.authorize(context.Background(), "secret.example", 443); err == nil {
		t.Fatal("private DNS result should be rejected")
	}
	if approvals != 1 || lookups != 1 {
		t.Fatalf("approval/lookups = %d/%d, want 1/1", approvals, lookups)
	}
}

func TestAuthorizeDoesNotResolveBeforeApproval(t *testing.T) {
	lookups := 0
	server := &Server{
		Rules: Rules{},
		Approve: func(context.Context, string, int) (Decision, error) {
			return Deny, nil
		},
		Resolve: func(context.Context, string) ([]net.IP, error) {
			lookups++
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		},
	}
	if _, _, err := server.authorize(context.Background(), "unknown.example", 443); err == nil {
		t.Fatal("denied host should fail")
	}
	if lookups != 0 {
		t.Fatalf("DNS lookup happened before approval: %d", lookups)
	}
}
