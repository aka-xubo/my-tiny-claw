package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

func sseBody(events ...string) io.ReadCloser {
	var b strings.Builder
	for _, e := range events {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &probe)
		b.WriteString("event: " + probe.Type + "\n")
		b.WriteString("data: " + e + "\n\n")
	}
	return io.NopCloser(strings.NewReader(b.String()))
}

func sseResponse(r *http.Request, events ...string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       sseBody(events...),
		Request:    r,
	}
}

const (
	evCreated   = `{"type":"response.created","sequence_number":0,"response":{"id":"resp-1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`
	evDeltaA    = `{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg-1","output_index":0,"content_index":0,"delta":"你好","logprobs":[]}`
	evDeltaB    = `{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg-1","output_index":0,"content_index":0,"delta":"，世界","logprobs":[]}`
	evCompleted = `{"type":"response.completed","sequence_number":3,"response":{"id":"resp-1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg-1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"你好，世界","annotations":[]}]},{"id":"fc-1","type":"function_call","status":"completed","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`
)

func newStreamTestProvider(t *testing.T, fn roundTripFunc) *OpenAIProvider {
	t.Helper()
	t.Setenv("CODEX_BASE_URL", "https://coding.gaiaworks.net/openai/v1")
	return newOpenAIProvider("m", "test-key", &http.Client{Transport: fn})
}

func TestGenerateStreamEmitsDeltasAndAggregatesCompletedResponse(t *testing.T) {
	p := newStreamTestProvider(t, func(r *http.Request) (*http.Response, error) {
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("解析请求体失败: %v", err)
		}
		if !body.Stream {
			t.Errorf("请求体 stream = false, want true")
		}
		return sseResponse(r, evCreated, evDeltaA, evDeltaB, evCompleted), nil
	})

	var deltas []string
	got, err := p.GenerateStream(context.Background(), []schema.Message{{Role: schema.RoleUser, Content: "hi"}}, nil,
		func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("GenerateStream() error = %v", err)
	}
	if strings.Join(deltas, "|") != "你好|，世界" {
		t.Fatalf("deltas = %q", deltas)
	}
	if got.Content != "你好，世界" {
		t.Fatalf("Content = %q", got.Content)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call_1" || string(got.ToolCalls[0].Arguments) != `{"city":"北京"}` {
		t.Fatalf("ToolCalls = %+v", got.ToolCalls)
	}
	if got.Usage == nil || got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 7 {
		t.Fatalf("Usage = %+v", got.Usage)
	}
}

func TestGenerateStreamReturnsErrorOnFailedEvent(t *testing.T) {
	failed := `{"type":"response.failed","sequence_number":2,"response":{"id":"resp-1","object":"response","created_at":1,"status":"failed","model":"m","output":[],"error":{"code":"server_error","message":"boom"}}}`
	var calls int32
	p := newStreamTestProvider(t, func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return sseResponse(r, evCreated, evDeltaA, failed), nil
	})
	_, err := p.GenerateStream(context.Background(), []schema.Message{{Role: schema.RoleUser, Content: "hi"}}, nil, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want contains boom", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("已收到事件后不应退回非流式重试，请求次数 = %d", n)
	}
}

func TestGenerateStreamErrorsWhenStreamEndsBeforeCompleted(t *testing.T) {
	var calls int32
	p := newStreamTestProvider(t, func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return sseResponse(r, evCreated, evDeltaA), nil
	})
	_, err := p.GenerateStream(context.Background(), []schema.Message{{Role: schema.RoleUser, Content: "hi"}}, nil, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("err = %v, want stream ended before completed", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("请求次数 = %d, want 1", n)
	}
}

func TestGenerateStreamFallsBackToGenerateWhenNoEventReceived(t *testing.T) {
	var streamCalls, plainCalls int32
	p := newStreamTestProvider(t, func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			// 模拟中转站不支持 SSE：流式请求直接连接失败（SDK 自身会重试几次）
			atomic.AddInt32(&streamCalls, 1)
			return nil, errors.New("connection refused")
		}
		atomic.AddInt32(&plainCalls, 1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       responsesTestBody(),
			Request:    r,
		}, nil
	})
	var deltas []string
	got, err := p.GenerateStream(context.Background(), []schema.Message{{Role: schema.RoleUser, Content: "hi"}}, nil,
		func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("GenerateStream() error = %v", err)
	}
	if got.Content != "ok" || strings.Join(deltas, "") != "ok" {
		t.Fatalf("Content = %q, deltas = %q", got.Content, deltas)
	}
	if atomic.LoadInt32(&streamCalls) == 0 || atomic.LoadInt32(&plainCalls) != 1 {
		t.Fatalf("streamCalls = %d, plainCalls = %d, want 先流式失败再退回一次非流式", streamCalls, plainCalls)
	}
}

func TestGenerateStreamDoesNotFallBackWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32
	p := newStreamTestProvider(t, func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		cancel()
		return nil, context.Canceled
	})
	_, err := p.GenerateStream(ctx, []schema.Message{{Role: schema.RoleUser, Content: "hi"}}, nil, func(string) {})
	if err == nil {
		t.Fatal("want error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("ctx 取消后不应退回，请求次数 = %d", n)
	}
}
