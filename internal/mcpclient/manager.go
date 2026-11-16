// internal/mcpclient/manager.go
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aka-xubo/my-tiny-claw/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// SDK 未导出这两个常量
	protocolVersionStateful  = "2025-11-25"
	protocolVersionStateless = "2026-07-28"
)

// Options 用于注入外部依赖，零值即默认行为
type Options struct {
	// CredentialPath 是 OAuth 凭据文件路径，默认 ~/.claw/mcp-oauth.json
	CredentialPath string
	// OpenBrowser 打开授权页面，默认调用系统浏览器
	OpenBrowser func(url string) error
}

// Manager 负责 MCP Server 的连接、工具发现和关闭
type Manager struct {
	cfg         *Config
	store       *credentialStore
	openBrowser func(string) error

	authSem   chan struct{} // 浏览器授权全局串行
	connected atomic.Bool   // Connect 结束后禁止再发起浏览器授权

	sessions []*mcp.ClientSession
	tools    []tools.BaseTool
	results  []serverResult
}

type serverResult struct {
	name            string
	protocolVersion string
	toolCount       int
	err             error
}

type serverConn struct {
	session         *mcp.ClientSession
	protocolVersion string
	tools           []*mcp.Tool
}

// NewManager 创建 Manager。cfg 为 nil 时表示没有 MCP Server。
func NewManager(cfg *Config, opts Options) (*Manager, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	credPath := opts.CredentialPath
	if credPath == "" {
		p, err := defaultCredentialPath()
		if err != nil {
			return nil, err
		}
		credPath = p
	}
	openBrowser := opts.OpenBrowser
	if openBrowser == nil {
		openBrowser = openSystemBrowser
	}
	return &Manager{
		cfg:         cfg,
		store:       &credentialStore{path: credPath},
		openBrowser: openBrowser,
		authSem:     make(chan struct{}, 1),
	}, nil
}

// Connect 并行连接所有 Server 并发现工具。单个 Server 失败只记录日志。
func (m *Manager) Connect(ctx context.Context) {
	names := make([]string, 0, len(m.cfg.Servers))
	for name := range m.cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	conns := make([]*serverConn, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conns[i], errs[i] = m.connectWithRetry(ctx, name, m.cfg.Servers[name])
		}()
	}
	wg.Wait()
	m.connected.Store(true)

	// 按 Server 名称顺序注册，保证冲突时"保留第一个"的结果稳定
	seen := make(map[string]string)
	for i, name := range names {
		res := serverResult{name: name, err: errs[i]}
		if errs[i] != nil {
			log.Printf("[MCP] ❌ server %s 连接失败: %v\n", name, errs[i])
			m.results = append(m.results, res)
			continue
		}
		conn := conns[i]
		res.protocolVersion = conn.protocolVersion
		m.sessions = append(m.sessions, conn.session)
		for _, rt := range m.buildTools(name, m.cfg.Servers[name], conn) {
			if prev, dup := seen[rt.name]; dup {
				log.Printf("[MCP] ⚠️ server %s 的工具 %s 与 %s 映射后同名 (%s)，已跳过\n", name, rt.remoteName, prev, rt.name)
				continue
			}
			seen[rt.name] = name + "/" + rt.remoteName
			m.tools = append(m.tools, rt)
			res.toolCount++
		}
		log.Printf("[MCP] ✅ server %s 已连接 (协议 %s)，注册 %d 个工具\n", name, res.protocolVersion, res.toolCount)
		m.results = append(m.results, res)
	}
}

// Tools 返回所有远程工具代理
func (m *Manager) Tools() []tools.BaseTool {
	return m.tools
}

// Close 关闭所有 Session，返回汇总错误
func (m *Manager) Close() error {
	var errs []error
	for _, s := range m.sessions {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	m.sessions = nil
	if len(errs) > 0 {
		log.Printf("[MCP] 关闭 Session 时出现错误: %v\n", errors.Join(errs...))
	} else if len(m.results) > 0 {
		log.Printf("[MCP] 所有 MCP Session 已关闭\n")
	}
	return errors.Join(errs...)
}

// connectWithRetry 在 OAuth 已保存客户端失效时，删除记录并重新 DCR 一次
func (m *Manager) connectWithRetry(ctx context.Context, name string, sc ServerConfig) (*serverConn, error) {
	conn, err := m.connectServer(ctx, name, sc)
	if err != nil && sc.OAuth != nil && isInvalidClient(err) {
		log.Printf("[MCP] server %s 的已保存 OAuth 客户端失效，重新注册\n", name)
		if derr := m.store.delete(sc.URL); derr != nil {
			return nil, derr
		}
		conn, err = m.connectServer(ctx, name, sc)
	}
	return conn, explainOAuthError(err)
}

func (m *Manager) connectServer(ctx context.Context, name string, sc ServerConfig) (*serverConn, error) {
	ctx, cancel := context.WithTimeout(ctx, sc.connectTimeout())
	defer cancel()

	transport := &mcp.StreamableClientTransport{
		Endpoint:             sc.URL,
		HTTPClient:           &http.Client{Transport: &headerRoundTripper{base: http.DefaultTransport, headers: sc.Headers}},
		DisableStandaloneSSE: true,
	}
	if sc.OAuth != nil {
		h, err := m.newOAuthHandler(sc)
		if err != nil {
			return nil, err
		}
		transport.OAuthHandler = h
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "my-tiny-claw", Version: "0.1.0"}, &mcp.ClientOptions{
		// 显式设置空能力，避免 SDK 默认声明 roots.listChanged
		Capabilities: &mcp.ClientCapabilities{},
	})
	var sessOpts *mcp.ClientSessionOptions
	if sc.Transport == TransportStreamableHTTP {
		sessOpts = &mcp.ClientSessionOptions{ProtocolVersion: protocolVersionStateful}
	}
	session, err := client.Connect(ctx, transport, sessOpts)
	if err != nil {
		return nil, err
	}
	version := ""
	if ir := session.InitializeResult(); ir != nil {
		version = ir.ProtocolVersion
	}
	if sc.Transport == TransportStatelessHTTP && version < protocolVersionStateless {
		log.Printf("[MCP] ⚠️ server %s 不支持无状态模式，已回退为有状态模式 (协议 %s)，Session 失效后不会自动重建\n", name, version)
	}

	var list []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			_ = session.Close()
			return nil, fmt.Errorf("获取工具列表失败: %w", err)
		}
		list = append(list, tool)
	}
	return &serverConn{session: session, protocolVersion: version, tools: list}, nil
}

// buildTools 应用白名单、校验 Schema 并生成代理
func (m *Manager) buildTools(server string, sc ServerConfig, conn *serverConn) []*RemoteTool {
	var allow map[string]bool
	if sc.Tools != nil {
		allow = make(map[string]bool, len(sc.Tools))
		for _, t := range sc.Tools {
			allow[t] = false
		}
	}
	var out []*RemoteTool
	for _, t := range conn.tools {
		if allow != nil {
			if _, ok := allow[t.Name]; !ok {
				continue
			}
			allow[t.Name] = true
		}
		schemaMap, err := objectSchema(t.InputSchema)
		if err != nil {
			log.Printf("[MCP] ⚠️ server %s 的工具 %s Schema 非法，已跳过: %v\n", server, t.Name, err)
			continue
		}
		out = append(out, &RemoteTool{
			name:        exposedName(server, t.Name),
			server:      server,
			remoteName:  t.Name,
			description: t.Description,
			inputSchema: schemaMap,
			session:     conn.session,
			callTimeout: sc.callTimeout(),
		})
	}
	for _, name := range sc.Tools {
		if !allow[name] {
			log.Printf("[MCP] ⚠️ server %s 的白名单工具 %s 不存在\n", server, name)
		}
	}
	return out
}

func objectSchema(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, errors.New("inputSchema 不是 JSON 对象")
	}
	if m["type"] != "object" {
		return nil, fmt.Errorf("inputSchema.type 必须是 object (got %v)", m["type"])
	}
	return m, nil
}

// headerRoundTripper 为 MCP 请求注入配置的 Header
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(h.headers) == 0 {
		return h.base.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// explainOAuthError 为常见 OAuth 失败补充修复提示
func explainOAuthError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "does not match pre-registered credentials issuer"):
		return fmt.Errorf("%w (授权服务器与已保存凭据不一致，如确认可信，请删除 ~/.claw/mcp-oauth.json 中对应记录)", err)
	case strings.Contains(msg, "no configured client registration methods"),
		strings.Contains(msg, "failed to register client"):
		return fmt.Errorf("%w (该授权服务器不支持动态客户端注册 DCR)", err)
	}
	return err
}

var _ tools.BaseTool = (*RemoteTool)(nil)
var _ tools.TraceRedactor = (*RemoteTool)(nil)
var _ auth.OAuthHandler = (*startupOnlyHandler)(nil)
