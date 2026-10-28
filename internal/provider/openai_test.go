package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func responsesTestBody() io.ReadCloser {
	return io.NopCloser(strings.NewReader(`{"id":"resp-test","object":"response","created_at":1,"status":"completed","model":"gpt-5.6-sol","output":[{"id":"msg-test","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}`))
}

func TestOpenAIProviderUsesResponsesEndpoint(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", "https://coding.gaiaworks.net/openai/v1")

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Errorf("请求方法 = %s, want POST", r.Method)
		}
		if r.URL.Path != "/openai/v1/responses" {
			t.Errorf("请求路径 = %s, want /openai/v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}

		var body struct {
			Model string `json:"model"`
			Input []struct {
				Role string `json:"role"`
				Type string `json:"type"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("解析请求体失败: %v", err)
		}
		if body.Model != "gpt-5.6-sol" {
			t.Errorf("请求模型 = %q, want gpt-5.6-sol", body.Model)
		}
		if len(body.Input) != 1 || body.Input[0].Role != "user" {
			t.Errorf("请求 input = %+v, want one user message", body.Input)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       responsesTestBody(),
			Request:    r,
		}, nil
	})

	p := newOpenAIProvider("gpt-5.6-sol", "test-key", &http.Client{Transport: transport})
	got, err := p.Generate(context.Background(), []schema.Message{{Role: schema.RoleUser, Content: "Hello"}}, nil)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if got.Content != "ok" {
		t.Fatalf("响应内容 = %q, want ok", got.Content)
	}
}

func TestOpenAIProviderEncodesResponsesFunctionCallAndOutput(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", "https://coding.gaiaworks.net/openai/v1")

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Input []struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Arguments string `json:"arguments"`
				Output    string `json:"output"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("解析请求体失败: %v", err)
		}

		var foundCall, foundOutput bool
		for _, item := range body.Input {
			switch item.Type {
			case "function_call":
				foundCall = true
				if item.CallID != "call_1" || item.Arguments != `{"city":"北京"}` {
					t.Errorf("function_call = %+v", item)
				}
			case "function_call_output":
				foundOutput = true
				if item.CallID != "call_1" || item.Output != "天气：晴" {
					t.Errorf("function_call_output = %+v", item)
				}
			}
		}
		if !foundCall || !foundOutput {
			t.Errorf("请求 input 缺少 function_call 或 function_call_output: %+v", body.Input)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       responsesTestBody(),
			Request:    r,
		}, nil
	})

	p := newOpenAIProvider("gpt-5.6-sol", "test-key", &http.Client{Transport: transport})
	_, err := p.Generate(context.Background(), []schema.Message{
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: []byte(`{"city":"北京"}`)}}},
		{Role: schema.RoleUser, ToolCallID: "call_1", Content: "天气：晴"},
	}, nil)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
}

func TestOpenAIProviderParsesResponsesFunctionCall(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", "https://coding.gaiaworks.net/openai/v1")
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp-test","object":"response","created_at":1,"status":"completed","model":"gpt-5.6-sol","output":[{"id":"fc-test","type":"function_call","status":"completed","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"}]}`)),
			Request:    r,
		}, nil
	})

	p := newOpenAIProvider("gpt-5.6-sol", "test-key", &http.Client{Transport: transport})
	got, err := p.Generate(context.Background(), []schema.Message{{Role: schema.RoleUser, Content: "查天气"}}, nil)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call_1" || got.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("工具调用 = %+v", got.ToolCalls)
	}
	if string(got.ToolCalls[0].Arguments) != `{"city":"北京"}` {
		t.Fatalf("工具参数 = %s", got.ToolCalls[0].Arguments)
	}
}
