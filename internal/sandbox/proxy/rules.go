package proxy

import (
	"fmt"
	"net"
	"strings"
)

type Decision int

const (
	NoMatch Decision = iota
	Allow
	Deny
)

type DomainRule string

const (
	RuleAllow DomainRule = "allow"
	RuleDeny  DomainRule = "deny"
)

type Rules map[string]DomainRule

func NormalizeHost(raw string) (string, net.IP, error) {
	host := strings.TrimSpace(raw)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	host = strings.ToLower(strings.TrimRight(host, "."))
	if host == "" {
		return "", nil, fmt.Errorf("host 不能为空")
	}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), v4, nil
		}
		return ip.String(), ip, nil
	}
	if strings.ContainsAny(host, " /\\\x00\r\n") {
		return "", nil, fmt.Errorf("host 格式非法")
	}
	if len(host) > 253 {
		return "", nil, fmt.Errorf("host 过长")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", nil, fmt.Errorf("host 格式非法")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", nil, fmt.Errorf("host 格式非法")
			}
		}
	}
	return host, nil, nil
}

func Match(host string, rules Rules) Decision {
	deny := false
	allow := false
	for rawPattern, rule := range rules {
		pattern := strings.ToLower(strings.TrimRight(strings.TrimSpace(rawPattern), "."))
		matched := false
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[2:]
			matched = host != suffix && strings.HasSuffix(host, "."+suffix)
		} else {
			matched = host == pattern
		}
		if !matched {
			continue
		}
		switch rule {
		case RuleDeny:
			deny = true
		case RuleAllow:
			allow = true
		}
	}
	if deny {
		return Deny
	}
	if allow {
		return Allow
	}
	return NoMatch
}

func IsPrivateOrLocal(ip net.IP) bool {
	return !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func NormalizeRules(input Rules) (Rules, error) {
	result := make(Rules, len(input))
	for raw, rule := range input {
		if rule != RuleAllow && rule != RuleDeny {
			return nil, fmt.Errorf("未知域名规则: %s", rule)
		}
		pattern := raw
		wildcard := strings.HasPrefix(pattern, "*.")
		if wildcard {
			pattern = pattern[2:]
		}
		host, ip, err := NormalizeHost(pattern)
		if err != nil {
			return nil, err
		}
		if wildcard {
			if ip != nil {
				return nil, fmt.Errorf("IP 不支持通配")
			}
			host = "*." + host
		}
		if old, ok := result[host]; !ok || old != RuleDeny {
			result[host] = rule
		}
	}
	return result, nil
}

func IsExplicitLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
