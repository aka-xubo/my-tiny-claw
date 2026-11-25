package fsops

import (
	"fmt"
	"strings"
)

// FuzzyReplace is the existing edit_file matching contract, kept in a small
// package so the sandboxed helper and the legacy tool share identical errors.
func FuzzyReplace(originalContent, oldText, newText string) (string, error) {
	count := strings.Count(originalContent, oldText)
	if count == 1 {
		return strings.Replace(originalContent, oldText, newText, 1), nil
	}
	if count > 1 {
		return "", fmt.Errorf("old_text 匹配到了 %d 处，请提供更多的上下文代码以确保唯一性", count)
	}

	normalizedContent := strings.ReplaceAll(originalContent, "\r\n", "\n")
	normalizedOld := strings.ReplaceAll(oldText, "\r\n", "\n")
	count = strings.Count(normalizedContent, normalizedOld)
	if count == 1 {
		return strings.Replace(normalizedContent, normalizedOld, newText, 1), nil
	}

	trimmedOld := strings.TrimSpace(normalizedOld)
	if trimmedOld != "" {
		count = strings.Count(normalizedContent, trimmedOld)
		if count == 1 {
			return strings.Replace(normalizedContent, trimmedOld, newText, 1), nil
		}
	}
	return lineByLineReplace(normalizedContent, normalizedOld, newText)
}

func lineByLineReplace(content, oldText, newText string) (string, error) {
	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(strings.TrimSpace(oldText), "\n")
	if len(oldLines) == 0 || len(contentLines) < len(oldLines) {
		return "", fmt.Errorf("找不到该代码片段")
	}
	for i := range oldLines {
		oldLines[i] = strings.TrimSpace(oldLines[i])
	}

	matches := 0
	start := -1
	end := -1
	for i := 0; i <= len(contentLines)-len(oldLines); i++ {
		matched := true
		for j := range oldLines {
			if strings.TrimSpace(contentLines[i+j]) != oldLines[j] {
				matched = false
				break
			}
		}
		if matched {
			matches++
			start, end = i, i+len(oldLines)
		}
	}
	if matches == 0 {
		return "", fmt.Errorf("在文件中未找到 old_text，请大模型先调用 read_file 仔细确认文件内容和缩进")
	}
	if matches > 1 {
		return "", fmt.Errorf("模糊匹配到了 %d 处相似代码，请提供更多上下行代码以精确定位", matches)
	}
	lines := make([]string, 0, len(contentLines)-len(oldLines)+1)
	lines = append(lines, contentLines[:start]...)
	lines = append(lines, newText)
	lines = append(lines, contentLines[end:]...)
	return strings.Join(lines, "\n"), nil
}
