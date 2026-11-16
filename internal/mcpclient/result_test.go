package mcpclient

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExposedName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	if got := exposedName("github", "create_issue"); got != "mcp__github__create_issue" {
		t.Fatalf("got %s", got)
	}
	if exposedName("s", "a.b") != exposedName("s", "a_b") {
		t.Fatal("清洗后应同名，由冲突检测处理")
	}
	long := strings.Repeat("x", 100)
	a, b := exposedName("srv", long+"1"), exposedName("srv", long+"2")
	if len(a) != 64 || !valid.MatchString(a) || a == b {
		t.Fatalf("超长名称应截断到 64 并带不同哈希: %s %s", a, b)
	}
	if a != exposedName("srv", long+"1") {
		t.Fatal("哈希应稳定")
	}
}

func TestFormatResult(t *testing.T) {
	cases := []struct {
		name string
		res  *mcp.CallToolResult
		want []string
		not  []string
	}{
		{"文本", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a"}, &mcp.TextContent{Text: "b"}}}, []string{"a\nb"}, nil},
		{"仅结构化", &mcp.CallToolResult{StructuredContent: map[string]any{"k": 1}}, []string{`{"k":1}`}, nil},
		{"文本加结构化", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "t"}}, StructuredContent: map[string]any{"k": 1}}, []string{"t"}, []string{`"k"`}},
		{"图片", &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: []byte("12345")}}}, []string{"image/png", "5 字节"}, []string{"MTIzNDU"}},
		{"音频", &mcp.CallToolResult{Content: []mcp.Content{&mcp.AudioContent{MIMEType: "audio/wav", Data: []byte("12")}}}, []string{"audio/wav", "2 字节"}, nil},
		{"文本资源", &mcp.CallToolResult{Content: []mcp.Content{&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///a", Text: "body"}}}}, []string{"file:///a", "body"}, nil},
		{"二进制资源", &mcp.CallToolResult{Content: []mcp.Content{&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///b", MIMEType: "application/pdf", Blob: []byte("xyz")}}}}, []string{"file:///b", "3 字节"}, nil},
		{"资源链接", &mcp.CallToolResult{Content: []mcp.Content{&mcp.ResourceLink{URI: "https://r", Name: "r"}}}, []string{"https://r"}, nil},
		{"空结果", &mcp.CallToolResult{}, []string{emptyResultText}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatResult(c.res)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Fatalf("结果缺少 %q: %s", w, got)
				}
			}
			for _, n := range c.not {
				if strings.Contains(got, n) {
					t.Fatalf("结果不应包含 %q: %s", n, got)
				}
			}
		})
	}
}

func TestTruncateMiddle(t *testing.T) {
	s := "头" + strings.Repeat("中", 30000) + "尾"
	got := truncateMiddle(s, maxOutputRunes)
	if utf8.RuneCountInString(got) > maxOutputRunes || !strings.HasPrefix(got, "头") || !strings.HasSuffix(got, "尾") || !strings.Contains(got, "截断") {
		t.Fatalf("截断结果不符合预期，长度 %d", utf8.RuneCountInString(got))
	}
	if truncateMiddle("short", 10) != "short" {
		t.Fatal("短文本不应截断")
	}
}
