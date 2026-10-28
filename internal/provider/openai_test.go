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

func TestOpenAIProviderUsesChatCompletionsEndpoint(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", "https://coding.gaiaworks.net/v1")

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Errorf("请求方法 = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("请求路径 = %s, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}

		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("解析请求体失败: %v", err)
		}
		if body.Model != "gpt-5.6-sol" {
			t.Errorf("请求模型 = %q, want gpt-5.6-sol", body.Model)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)),
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
