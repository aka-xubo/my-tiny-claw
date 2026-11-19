package observability

import (
	"context"
	"strings"
	"testing"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

type plainProvider struct{}

func (plainProvider) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (*schema.Message, error) {
	return &schema.Message{Role: schema.RoleAssistant, Content: "整段", Usage: &schema.Usage{PromptTokens: 3, CompletionTokens: 2}}, nil
}

type streamProvider struct{ plainProvider }

func (streamProvider) GenerateStream(_ context.Context, _ []schema.Message, _ []schema.ToolDefinition, onDelta provider.StreamHandler) (*schema.Message, error) {
	onDelta("流")
	onDelta("式")
	return &schema.Message{Role: schema.RoleAssistant, Content: "流式", Usage: &schema.Usage{PromptTokens: 5, CompletionTokens: 4}}, nil
}

func TestCostTrackerGenerateStreamPassesThrough(t *testing.T) {
	sess := cctx.NewSession("s", t.TempDir())
	tr := NewCostTracker(streamProvider{}, "gpt-6.1-sol", sess)
	var deltas []string
	got, err := tr.GenerateStream(context.Background(), nil, nil, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "流式" || strings.Join(deltas, "|") != "流|式" {
		t.Fatalf("Content = %q, deltas = %q", got.Content, deltas)
	}
	if sess.TotalPromptTokens != 5 || sess.TotalCompletionTokens != 4 {
		t.Fatalf("usage 未记账: %d/%d", sess.TotalPromptTokens, sess.TotalCompletionTokens)
	}
}

func TestCostTrackerGenerateStreamFallsBackForPlainProvider(t *testing.T) {
	sess := cctx.NewSession("s", t.TempDir())
	tr := NewCostTracker(plainProvider{}, "gpt-6.1-sol", sess)
	var deltas []string
	got, err := tr.GenerateStream(context.Background(), nil, nil, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "整段" || strings.Join(deltas, "|") != "整段" {
		t.Fatalf("Content = %q, deltas = %q", got.Content, deltas)
	}
	if sess.TotalPromptTokens != 3 {
		t.Fatalf("usage 未记账: %d", sess.TotalPromptTokens)
	}
}
