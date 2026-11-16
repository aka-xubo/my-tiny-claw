// internal/mcpclient/result.go
package mcpclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxToolNameLen  = 64
	maxOutputRunes  = 20000
	emptyResultText = "工具执行成功但没有文本输出。"
)

var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// exposedName 生成暴露给模型的工具名 mcp__<server>__<tool>，满足 ^[a-zA-Z0-9_-]{1,64}$
func exposedName(server, tool string) string {
	name := "mcp__" + server + "__" + invalidNameChars.ReplaceAllString(tool, "_")
	if len(name) <= maxToolNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]
	return name[:maxToolNameLen-len(suffix)] + suffix
}

// formatResult 把 CallToolResult 转换成模型可读的文本
func formatResult(res *mcp.CallToolResult) string {
	var parts []string
	hasText := false
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			hasText = true
			parts = append(parts, v.Text)
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[图片: %s, %d 字节]", v.MIMEType, len(v.Data)))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[音频: %s, %d 字节]", v.MIMEType, len(v.Data)))
		case *mcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[资源链接: %s (%s)]", v.URI, v.Name))
		case *mcp.EmbeddedResource:
			if r := v.Resource; r != nil {
				if r.Blob != nil {
					parts = append(parts, fmt.Sprintf("[资源: %s, %s, %d 字节]", r.URI, r.MIMEType, len(r.Blob)))
				} else {
					parts = append(parts, fmt.Sprintf("[资源: %s]\n%s", r.URI, r.Text))
				}
			}
		default:
			parts = append(parts, fmt.Sprintf("[不支持的内容类型: %T]", c))
		}
	}
	// 有文本时忽略 StructuredContent，避免重复
	if !hasText && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append([]string{string(b)}, parts...)
		}
	}
	if len(parts) == 0 {
		return emptyResultText
	}
	return truncateMiddle(strings.Join(parts, "\n"), maxOutputRunes)
}

// truncateMiddle 超长时保留开头和结尾，并提示模型内容被截断
func truncateMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	notice := fmt.Sprintf("\n...[结果过长，系统已截断，原始长度 %d 字符]...\n", len(r))
	keep := max - len([]rune(notice))
	head := keep / 2
	tail := keep - head
	return string(r[:head]) + notice + string(r[len(r)-tail:])
}
