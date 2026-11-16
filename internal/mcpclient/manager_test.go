package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/observability"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// requestLog 记录测试 Server 收到的 HTTP 请求
type requestLog struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (l *requestLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.reqs = append(l.reqs, r.Clone(context.Background()))
		l.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

func (l *requestLog) snapshot() []*http.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*http.Request(nil), l.reqs...)
}

var objSchema = map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}}

// newTestMCPServer 创建测试 MCP Server。versions 为空表示支持 SDK 全部协议版本。
func newTestMCPServer(stateless bool, versions []string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: versions})
	s.AddTool(&mcp.Tool{Name: "echo", Description: "回显 msg", InputSchema: objSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Msg string }
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + args.Msg + ":" + string(req.Params.Arguments)}}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "fail", InputSchema: objSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom"}}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "slow", InputSchema: objSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "late"}}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "dotted.name", InputSchema: objSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "dotted"}}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "dotted_name", InputSchema: objSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "underscore"}}}, nil
	})
	return s
}

func startMCPServer(t *testing.T, stateless bool, versions []string) (*httptest.Server, *requestLog) {
	t.Helper()
	srv := newTestMCPServer(stateless, versions)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: stateless})
	rl := &requestLog{}
	ts := httptest.NewServer(rl.wrap(h))
	t.Cleanup(ts.Close)
	return ts, rl
}

// captureLog 捕获标准库 log 输出
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestManager(t *testing.T, servers map[string]ServerConfig) *Manager {
	t.Helper()
	m, err := NewManager(&Config{Servers: servers}, Options{
		CredentialPath: filepath.Join(t.TempDir(), "mcp-oauth.json"),
		OpenBrowser:    func(string) error { return fmt.Errorf("测试中不应打开浏览器") },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func findTool(t *testing.T, m *Manager, name string) tools.BaseTool {
	t.Helper()
	for _, tool := range m.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("找不到工具 %s", name)
	return nil
}

func protocolOf(m *Manager, server string) string {
	for _, r := range m.results {
		if r.name == server {
			return r.protocolVersion
		}
	}
	return ""
}

func TestProtocolConstantsSupportedBySDK(t *testing.T) {
	supported := strings.Join(mcp.SupportedProtocolVersions(), ",")
	for _, v := range []string{protocolVersionStateful, protocolVersionStateless} {
		if !strings.Contains(supported, v) {
			t.Fatalf("SDK 不再支持协议版本 %s", v)
		}
	}
}

func TestStreamableHTTPMode(t *testing.T) {
	ts, rl := startMCPServer(t, false, nil)
	m := newTestManager(t, map[string]ServerConfig{
		"stateful": {Transport: TransportStreamableHTTP, URL: ts.URL, Headers: map[string]string{"X-Test": "hello"}},
	})
	m.Connect(context.Background())
	if v := protocolOf(m, "stateful"); v != protocolVersionStateful {
		t.Fatalf("协商版本应为 %s, got %q", protocolVersionStateful, v)
	}
	out, err := findTool(t, m, "mcp__stateful__echo").Execute(context.Background(), json.RawMessage(`{"msg":"hi"}`))
	if err != nil || !strings.Contains(out, "echo:hi") {
		t.Fatalf("调用失败: %q %v", out, err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	sawSession, sawDelete := false, false
	for _, r := range rl.snapshot() {
		if r.Header.Get("X-Test") != "hello" {
			t.Fatalf("%s 请求缺少自定义 Header", r.Method)
		}
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") != "" {
			sawSession = true
		}
		if r.Method == http.MethodGet {
			t.Fatal("已禁用独立 SSE，不应发送 GET")
		}
		if r.Method == http.MethodDelete {
			sawDelete = true
		}
	}
	if !sawSession || !sawDelete {
		t.Fatalf("有状态模式应携带 Session 并在关闭时 DELETE: session=%v delete=%v", sawSession, sawDelete)
	}
}

func TestStatelessHTTPMode(t *testing.T) {
	ts, rl := startMCPServer(t, true, nil)
	m := newTestManager(t, map[string]ServerConfig{
		"nostate": {Transport: TransportStatelessHTTP, URL: ts.URL},
	})
	logs := captureLog(t)
	m.Connect(context.Background())
	if v := protocolOf(m, "nostate"); v != protocolVersionStateless {
		t.Fatalf("协商版本应为 %s, got %q", protocolVersionStateless, v)
	}
	if strings.Contains(logs.String(), "回退") {
		t.Fatal("支持无状态的 Server 不应产生回退警告")
	}
	out, err := findTool(t, m, "mcp__nostate__echo").Execute(context.Background(), nil)
	if err != nil || !strings.Contains(out, "echo::{}") {
		t.Fatalf("空参数应转换为 {}: %q %v", out, err)
	}
	for _, r := range rl.snapshot() {
		if r.Header.Get("Mcp-Session-Id") != "" {
			t.Fatal("无状态模式不应携带 Mcp-Session-Id")
		}
	}
}

func TestStatelessFallsBackToStateful(t *testing.T) {
	ts, _ := startMCPServer(t, false, []string{protocolVersionStateful})
	m := newTestManager(t, map[string]ServerConfig{
		"legacy": {Transport: TransportStatelessHTTP, URL: ts.URL},
	})
	logs := captureLog(t)
	m.Connect(context.Background())
	if v := protocolOf(m, "legacy"); v != protocolVersionStateful {
		t.Fatalf("应回退到 %s, got %q (err=%v)", protocolVersionStateful, v, m.results)
	}
	if !strings.Contains(logs.String(), "已回退为有状态模式") {
		t.Fatalf("缺少回退警告: %s", logs.String())
	}
	if _, err := findTool(t, m, "mcp__legacy__echo").Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestToolMappingWhitelistAndConflicts(t *testing.T) {
	ts1, _ := startMCPServer(t, true, nil)
	ts2, _ := startMCPServer(t, true, nil)
	m := newTestManager(t, map[string]ServerConfig{
		"a": {Transport: TransportStatelessHTTP, URL: ts1.URL},
		"b": {Transport: TransportStatelessHTTP, URL: ts2.URL, Tools: []string{"echo", "missing"}},
	})
	logs := captureLog(t)
	m.Connect(context.Background())

	names := map[string]bool{}
	for _, tool := range m.Tools() {
		names[tool.Name()] = true
		def := tool.Definition()
		if def.Name != tool.Name() {
			t.Fatal("Definition 名称不一致")
		}
	}
	for _, want := range []string{"mcp__a__echo", "mcp__a__fail", "mcp__a__dotted_name", "mcp__b__echo"} {
		if !names[want] {
			t.Fatalf("缺少工具 %s, got %v", want, names)
		}
	}
	if names["mcp__b__fail"] {
		t.Fatal("白名单外的工具不应注册")
	}
	if !strings.Contains(logs.String(), "白名单工具 missing 不存在") {
		t.Fatal("白名单中不存在的工具应产生警告")
	}
	if !strings.Contains(logs.String(), "映射后同名") {
		t.Fatal("清洗后同名的工具应产生冲突警告")
	}
	// 冲突时保留 Server 返回的第一个
	out, err := findTool(t, m, "mcp__a__dotted_name").Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "dotted" && out != "underscore" {
		t.Fatalf("unexpected %q", out)
	}

	def := findTool(t, m, "mcp__a__echo").Definition()
	schemaMap := def.InputSchema.(map[string]any)
	if def.Description != "回显 msg" || schemaMap["type"] != "object" {
		t.Fatalf("Schema 映射错误: %+v", def)
	}
}

func TestToolErrorsAndTimeout(t *testing.T) {
	ts, _ := startMCPServer(t, true, nil)
	m := newTestManager(t, map[string]ServerConfig{
		"s": {Transport: TransportStatelessHTTP, URL: ts.URL, CallTimeoutSeconds: 1},
	})
	m.Connect(context.Background())

	_, err := findTool(t, m, "mcp__s__fail").Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "s/fail") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("IsError 应转换为错误并包含 Server、工具名和内容: %v", err)
	}
	_, err = findTool(t, m, "mcp__s__echo").Execute(context.Background(), json.RawMessage(`[1]`))
	if err == nil || !strings.Contains(err.Error(), "JSON 对象") {
		t.Fatalf("非对象参数应报错: %v", err)
	}

	start := time.Now()
	_, err = findTool(t, m, "mcp__s__slow").Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "超时") || time.Since(start) > 4*time.Second {
		t.Fatalf("应在调用超时后返回: %v (%s)", err, time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, err = findTool(t, m, "mcp__s__slow").Execute(ctx, nil)
	if err == nil || strings.Contains(err.Error(), "超时") {
		t.Fatalf("取消应返回非超时错误: %v", err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	for _, mode := range []struct {
		transport string
		stateless bool
	}{{TransportStreamableHTTP, false}, {TransportStatelessHTTP, true}} {
		t.Run(mode.transport, func(t *testing.T) {
			ts, _ := startMCPServer(t, mode.stateless, nil)
			m := newTestManager(t, map[string]ServerConfig{"s": {Transport: mode.transport, URL: ts.URL}})
			m.Connect(context.Background())
			tool := findTool(t, m, "mcp__s__echo")
			var wg sync.WaitGroup
			errs := make(chan error, 20)
			for i := range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					out, err := tool.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"msg":"m%d"}`, i)))
					if err != nil {
						errs <- err
					} else if !strings.Contains(out, fmt.Sprintf("echo:m%d:", i)) {
						errs <- fmt.Errorf("结果串号: %s", out)
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
		})
	}
}

func TestParallelConnectWithUnavailableServer(t *testing.T) {
	ok, _ := startMCPServer(t, true, nil)
	stop := make(chan struct{})
	hangHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	})
	hang := httptest.NewServer(hangHandler)
	t.Cleanup(hang.Close)
	hang2 := httptest.NewServer(hangHandler)
	t.Cleanup(hang2.Close)
	t.Cleanup(func() { close(stop) }) // 先于 Close 执行，释放挂起的请求

	m := newTestManager(t, map[string]ServerConfig{
		"good":  {Transport: TransportStatelessHTTP, URL: ok.URL},
		"hang1": {Transport: TransportStatelessHTTP, URL: hang.URL, ConnectTimeoutSeconds: 1},
		"hang2": {Transport: TransportStatelessHTTP, URL: hang2.URL, ConnectTimeoutSeconds: 1},
		"down":  {Transport: TransportStatelessHTTP, URL: "http://127.0.0.1:1"},
	})
	start := time.Now()
	m.Connect(context.Background())
	// 单个挂起 Server 耗时 = 连接超时 1s + SDK 关闭时等待取消通知最多 5s。
	// 串行会是 2 × 6s = 12s，并行应接近 6s。
	if d := time.Since(start); d > 9*time.Second {
		t.Fatalf("并行连接耗时 %s，超时不应累加", d)
	}
	if len(m.Tools()) == 0 {
		t.Fatal("可用 Server 的工具应正常注册")
	}
	failed := 0
	for _, r := range m.results {
		if r.err != nil {
			failed++
		}
	}
	if failed != 3 {
		t.Fatalf("应有 3 个 Server 失败, got %d", failed)
	}

	// 内置工具与 MCP 工具共存于 Registry，Schema 能交给 Provider
	reg := tools.NewRegistry()
	reg.Register(&builtinStub{})
	for _, tool := range m.Tools() {
		reg.Register(tool)
	}
	res := reg.Execute(context.Background(), schema.ToolCall{ID: "1", Name: "builtin"})
	if res.IsError || res.Output != "ok" {
		t.Fatalf("内置工具应不受影响: %+v", res)
	}
	found := false
	for _, d := range reg.GetAvailableTools() {
		if d.Name == "mcp__good__echo" {
			found = true
		}
	}
	if !found {
		t.Fatal("Registry 应暴露 MCP 工具 Schema")
	}
}

func TestRegistryRedactsMCPArgs(t *testing.T) {
	ts, _ := startMCPServer(t, true, nil)
	m := newTestManager(t, map[string]ServerConfig{"s": {Transport: TransportStatelessHTTP, URL: ts.URL}})
	m.Connect(context.Background())
	reg := tools.NewRegistry()
	for _, tool := range m.Tools() {
		reg.Register(tool)
	}
	ctx, root := observability.StartSpan(context.Background(), "root")
	res := reg.Execute(ctx, schema.ToolCall{ID: "1", Name: "mcp__s__echo", Arguments: json.RawMessage(`{"msg":"topsecret"}`)})
	if res.IsError {
		t.Fatal(res.Output)
	}
	b, _ := json.Marshal(root)
	if strings.Contains(string(b), "topsecret") {
		t.Fatalf("Trace 中不应出现 MCP 参数或输出: %s", b)
	}
}

type builtinStub struct{}

func (builtinStub) Name() string                      { return "builtin" }
func (builtinStub) Definition() schema.ToolDefinition { return schema.ToolDefinition{Name: "builtin"} }
func (builtinStub) Execute(context.Context, json.RawMessage) (string, error) {
	return "ok", nil
}
