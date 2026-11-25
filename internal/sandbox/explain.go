package sandbox

import (
	"fmt"
	"regexp"
)

// ExplainAccess is diagnostic only. Seatbelt remains the enforcement boundary.
func (p *Profile) ExplainAccess(raw string) (Access, string, error) {
	if p.FullAccess {
		return AccessWrite, "完全访问", nil
	}
	path, err := NormalizePath(raw)
	if err != nil {
		return AccessDeny, "", err
	}
	rules, err := canonicalRules(p.Files)
	if err != nil {
		return AccessDeny, "", err
	}
	var best *FileRule
	approved := AccessDeny
	approvedReason := ""
	for i := range rules {
		rule := &rules[i]
		matches := isStrictChildOrEqual(rule.Path, path)
		if rule.Glob != "" {
			expression, err := GlobToRegex(rule.Path, rule.Glob)
			if err != nil {
				return AccessDeny, "", err
			}
			matches, err = regexp.MatchString(expression, path)
			if err != nil {
				return AccessDeny, "", err
			}
		}
		if !matches {
			continue
		}
		if rule.Approved {
			if rule.Access > approved {
				approved = rule.Access
				approvedReason = rule.Path
			}
			continue
		}
		if best == nil || higherPriority(*rule, *best) {
			best = rule
		}
	}
	access := AccessDeny
	reason := "没有匹配的放行规则"
	if best != nil {
		access = best.Access
		reason = best.Path
		if best.Glob != "" {
			reason += "/" + best.Glob
		}
		reason = fmt.Sprintf("%s = %s", reason, []string{"deny", "read", "write"}[access])
	}
	if approved > access {
		access = approved
		reason = "用户批准的额外权限：" + approvedReason
	}
	return access, reason, nil
}
