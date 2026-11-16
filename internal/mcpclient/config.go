// internal/mcpclient/config.go
package mcpclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	TransportStreamableHTTP = "streamable-http"
	TransportStatelessHTTP  = "stateless-http"

	defaultConnectTimeout = 15 * time.Second
	defaultCallTimeout    = 60 * time.Second
	defaultAuthTimeout    = 300 * time.Second
	defaultRedirectPort   = 19876
)

// Config 对应 ~/.claw/mcp.json
type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

// ServerConfig 描述单个 MCP Server
type ServerConfig struct {
	Transport             string            `json:"transport"`
	URL                   string            `json:"url"`
	Headers               map[string]string `json:"headers,omitempty"`
	Tools                 []string          `json:"tools,omitempty"`
	OAuth                 *OAuthConfig      `json:"oauth,omitempty"`
	ConnectTimeoutSeconds int               `json:"connect_timeout_seconds,omitempty"`
	CallTimeoutSeconds    int               `json:"call_timeout_seconds,omitempty"`
}

// OAuthConfig 存在即表示该 Server 使用 OAuth 授权码流程 + DCR
type OAuthConfig struct {
	RedirectPort       int `json:"redirect_port,omitempty"`
	AuthTimeoutSeconds int `json:"auth_timeout_seconds,omitempty"`
}

var serverNamePattern = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// DefaultConfigPath 返回 ~/.claw/mcp.json
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claw", "mcp.json"), nil
}

// LoadConfig 读取并校验配置。文件不存在时返回 (nil, nil)，表示没有 MCP Server。
// 返回的配置中 headers 已完成环境变量展开。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 MCP 配置 %s 失败: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析 MCP 配置 %s 失败: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("配置文件包含多个 JSON 值")
		}
		return nil, fmt.Errorf("解析 MCP 配置 %s 失败: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("MCP 配置 %s 不合法: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sc := c.Servers[name]
		if err := sc.validate(name); err != nil {
			return fmt.Errorf("server %q: %w", name, err)
		}
		c.Servers[name] = sc
	}
	return nil
}

func (sc *ServerConfig) validate(name string) error {
	if !serverNamePattern.MatchString(name) {
		return errors.New("名称只允许字母、数字和 '-'")
	}
	if sc.Transport != TransportStreamableHTTP && sc.Transport != TransportStatelessHTTP {
		return fmt.Errorf("transport 必须是 %q 或 %q", TransportStreamableHTTP, TransportStatelessHTTP)
	}
	u, err := url.Parse(sc.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url 必须是合法的 http 或 https 地址")
	}
	if sc.ConnectTimeoutSeconds < 0 || sc.CallTimeoutSeconds < 0 {
		return errors.New("超时时间不能为负数")
	}
	for k, v := range sc.Headers {
		expanded, err := expandEnv(v)
		if err != nil {
			return fmt.Errorf("header %q: %w", k, err)
		}
		sc.Headers[k] = expanded
	}
	if sc.OAuth != nil {
		for k := range sc.Headers {
			if strings.EqualFold(k, "Authorization") {
				return errors.New("oauth 与 headers 中的 Authorization 不能同时配置")
			}
		}
		if sc.OAuth.RedirectPort < 0 || sc.OAuth.RedirectPort > 65535 {
			return errors.New("oauth.redirect_port 超出范围")
		}
		if sc.OAuth.AuthTimeoutSeconds < 0 {
			return errors.New("oauth.auth_timeout_seconds 不能为负数")
		}
	}
	if u.Scheme == "http" && (len(sc.Headers) > 0 || sc.OAuth != nil) {
		log.Printf("[MCP] ⚠️ server %s 使用 http 传输凭据，凭据将以明文发送\n", name)
	}
	return nil
}

// expandEnv 展开 ${NAME}，引用不存在的变量时报错（错误中只包含变量名，不含值）
func expandEnv(s string) (string, error) {
	var missing []string
	out := envRefPattern.ReplaceAllStringFunc(s, func(m string) string {
		key := envRefPattern.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(key)
		if !ok {
			missing = append(missing, key)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("引用了不存在的环境变量: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func (sc ServerConfig) connectTimeout() time.Duration {
	if sc.OAuth != nil {
		if sc.OAuth.AuthTimeoutSeconds > 0 {
			return time.Duration(sc.OAuth.AuthTimeoutSeconds) * time.Second
		}
		return defaultAuthTimeout
	}
	if sc.ConnectTimeoutSeconds > 0 {
		return time.Duration(sc.ConnectTimeoutSeconds) * time.Second
	}
	return defaultConnectTimeout
}

func (sc ServerConfig) callTimeout() time.Duration {
	if sc.CallTimeoutSeconds > 0 {
		return time.Duration(sc.CallTimeoutSeconds) * time.Second
	}
	return defaultCallTimeout
}

func (sc ServerConfig) redirectURL() string {
	port := defaultRedirectPort
	if sc.OAuth != nil && sc.OAuth.RedirectPort > 0 {
		port = sc.OAuth.RedirectPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d/callback", port)
}
