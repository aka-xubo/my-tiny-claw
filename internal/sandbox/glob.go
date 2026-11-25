package sandbox

import (
	"fmt"
	"regexp"
	"strings"
)

func ValidateGlob(glob string) error {
	if strings.ContainsAny(glob, "{}[]") {
		return fmt.Errorf("glob 不支持 {} 和 []：%q", glob)
	}
	if strings.ContainsRune(glob, '\x00') || strings.ContainsAny(glob, "\r\n") {
		return fmt.Errorf("glob 不能包含 NUL 或换行")
	}
	return nil
}

// GlobToRegex converts the v1 glob syntax to an anchored regular expression.
// The caller supplies an absolute root and a glob relative to that root.
func GlobToRegex(root, glob string) (string, error) {
	if err := ValidateGlob(glob); err != nil {
		return "", err
	}
	root = strings.TrimRight(root, "/")
	var b strings.Builder
	b.WriteString("^")
	b.WriteString(regexp.QuoteMeta(root))
	b.WriteString("/")
	for i := 0; i < len(glob); {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i += 2
				if i < len(glob) && glob[i] == '/' {
					b.WriteString("(.*/)?")
					i++
				} else {
					b.WriteString(".*")
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
		i++
	}
	b.WriteString("(/.*)?$")
	return b.String(), nil
}
