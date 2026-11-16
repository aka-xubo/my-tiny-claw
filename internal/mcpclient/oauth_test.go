package mcpclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// fakeAuthServer 是测试用 OAuth 授权服务器，支持 DCR、PKCE、授权码和刷新
type fakeAuthServer struct {
	*httptest.Server
	issuer string

	noDCR      atomic.Bool
	denyAuth   atomic.Bool
	tokenTTL   atomic.Int64 // 秒
	registers  atomic.Int32
	authorizes atomic.Int32
	refreshes  atomic.Int32

	mu       sync.Mutex
	clients  map[string]bool
	codes    map[string]codeGrant
	access   map[string]time.Time // access token -> 过期时间
	refresh  map[string]string    // refresh token -> client
	seenSecr []string             // 授权服务器下发过的所有敏感值，用于日志检查
}

type codeGrant struct {
	client, challenge, redirect string
}

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	a := &fakeAuthServer{
		clients: map[string]bool{},
		codes:   map[string]codeGrant{},
		access:  map[string]time.Time{},
		refresh: map[string]string{},
	}
	a.tokenTTL.Store(3600)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		meta := map[string]any{
			"issuer":                           a.issuer,
			"authorization_endpoint":           a.issuer + "/authorize",
			"token_endpoint":                   a.issuer + "/token",
			"response_types_supported":         []string{"code"},
			"code_challenge_methods_supported": []string{"S256"},
			"scopes_supported":                 []string{"mcp", "offline_access"},
		}
		if !a.noDCR.Load() {
			meta["registration_endpoint"] = a.issuer + "/register"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if a.noDCR.Load() {
			http.NotFound(w, r)
			return
		}
		a.registers.Add(1)
		var md oauthex.ClientRegistrationMetadata
		json.NewDecoder(r.Body).Decode(&md)
		id := "client-" + rand.Text()[:8]
		a.mu.Lock()
		a.clients[id] = true
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"client_id": id, "redirect_uris": md.RedirectURIs, "token_endpoint_auth_method": "none",
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		a.authorizes.Add(1)
		q := r.URL.Query()
		redirect, _ := url.Parse(q.Get("redirect_uri"))
		rq := redirect.Query()
		rq.Set("state", q.Get("state"))
		a.mu.Lock()
		known := a.clients[q.Get("client_id")]
		a.mu.Unlock()
		if a.denyAuth.Load() {
			rq.Set("error", "access_denied")
		} else if !known {
			rq.Set("error", "invalid_client")
		} else {
			code := "code-" + rand.Text()[:8]
			a.mu.Lock()
			a.codes[code] = codeGrant{client: q.Get("client_id"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
			a.mu.Unlock()
			rq.Set("code", code)
		}
		redirect.RawQuery = rq.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		client := r.Form.Get("client_id")
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.clients[client] {
			tokenError(w, "invalid_client")
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			g, ok := a.codes[r.Form.Get("code")]
			delete(a.codes, r.Form.Get("code"))
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || g.client != client || g.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) || g.redirect != r.Form.Get("redirect_uri") {
				tokenError(w, "invalid_grant")
				return
			}
		case "refresh_token":
			if a.refresh[r.Form.Get("refresh_token")] != client {
				tokenError(w, "invalid_grant")
				return
			}
			delete(a.refresh, r.Form.Get("refresh_token"))
			a.refreshes.Add(1)
		default:
			tokenError(w, "unsupported_grant_type")
			return
		}
		at, rt := "at-"+rand.Text(), "rt-"+rand.Text()
		ttl := a.tokenTTL.Load()
		a.access[at] = time.Now().Add(time.Duration(ttl) * time.Second)
		a.refresh[rt] = client
		a.seenSecr = append(a.seenSecr, at, rt)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": at, "refresh_token": rt, "token_type": "Bearer", "expires_in": ttl,
		})
	})
	a.Server = httptest.NewServer(mux)
	a.issuer = a.Server.URL
	t.Cleanup(a.Server.Close)
	return a
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `{"error":%q}`, code)
}

func (a *fakeAuthServer) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	a.mu.Lock()
	exp, ok := a.access[token]
	a.mu.Unlock()
	if !ok || time.Now().After(exp) {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{Expiration: exp, Scopes: []string{"mcp"}}, nil
}

// forgetClients 模拟授权服务器删除了所有已注册客户端
func (a *fakeAuthServer) forgetClients() {
	a.mu.Lock()
	a.clients = map[string]bool{}
	a.access = map[string]time.Time{}
	a.refresh = map[string]string{}
	a.mu.Unlock()
}

func (a *fakeAuthServer) revokeAll() {
	a.mu.Lock()
	a.access = map[string]time.Time{}
	a.refresh = map[string]string{}
	a.mu.Unlock()
}

// startProtectedMCPServer 启动受 OAuth 保护的无状态 MCP Server
func startProtectedMCPServer(t *testing.T, a *fakeAuthServer) string {
	t.Helper()
	srv := newTestMCPServer(true, nil)
	mux := http.NewServeMux()
	ts := httptest.NewUnstartedServer(mux)
	ts.Start()
	t.Cleanup(ts.Close)
	resource := ts.URL + "/mcp"
	prmURL := ts.URL + "/.well-known/oauth-protected-resource/mcp"
	mux.Handle("/.well-known/oauth-protected-resource/mcp", auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:             resource,
		AuthorizationServers: []string{a.issuer},
	}))
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	mux.Handle("/mcp", auth.RequireBearerToken(a.verify, &auth.RequireBearerTokenOptions{ResourceMetadataURL: prmURL})(h))
	return resource
}

// browserFollower 模拟用户在浏览器中完成授权：GET 授权地址并跟随重定向到本地回调
func browserFollower(opened *atomic.Int32) func(string) error {
	return func(u string) error {
		opened.Add(1)
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type oauthEnv struct {
	t        *testing.T
	authSrv  *fakeAuthServer
	mcpURL   string
	credPath string
	port     int
	opened   atomic.Int32
}

func newOAuthEnv(t *testing.T) *oauthEnv {
	a := newFakeAuthServer(t)
	return &oauthEnv{
		t:        t,
		authSrv:  a,
		mcpURL:   startProtectedMCPServer(t, a),
		credPath: filepath.Join(t.TempDir(), "claw", "mcp-oauth.json"),
		port:     freePort(t),
	}
}

func (e *oauthEnv) serverConfig() ServerConfig {
	return ServerConfig{
		Transport: TransportStatelessHTTP,
		URL:       e.mcpURL,
		OAuth:     &OAuthConfig{RedirectPort: e.port, AuthTimeoutSeconds: 10},
	}
}

// start 模拟一次 claw 启动
func (e *oauthEnv) start() *Manager {
	e.t.Helper()
	m, err := NewManager(&Config{Servers: map[string]ServerConfig{"secure": e.serverConfig()}}, Options{
		CredentialPath: e.credPath,
		OpenBrowser:    browserFollower(&e.opened),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = m.Close() })
	m.Connect(context.Background())
	return m
}

func (e *oauthEnv) record() *oauthRecord {
	e.t.Helper()
	rec, err := (&credentialStore{path: e.credPath}).get(e.mcpURL)
	if err != nil {
		e.t.Fatal(err)
	}
	return rec
}

func mustConnected(t *testing.T, m *Manager) {
	t.Helper()
	for _, r := range m.results {
		if r.err != nil {
			t.Fatalf("server %s 连接失败: %v", r.name, r.err)
		}
	}
	out, err := findTool(t, m, "mcp__secure__echo").Execute(context.Background(), json.RawMessage(`{"msg":"x"}`))
	if err != nil || !strings.Contains(out, "echo:x") {
		t.Fatalf("工具调用失败: %q %v", out, err)
	}
}

func resultErr(m *Manager) error {
	for _, r := range m.results {
		if r.err != nil {
			return r.err
		}
	}
	return nil
}

func TestOAuthFirstConnectWithDCR(t *testing.T) {
	e := newOAuthEnv(t)
	logs := captureLog(t)
	m := e.start()
	mustConnected(t, m)

	if e.authSrv.registers.Load() != 1 || e.opened.Load() != 1 {
		t.Fatalf("首次连接应注册 1 次、打开浏览器 1 次: register=%d open=%d", e.authSrv.registers.Load(), e.opened.Load())
	}
	rec := e.record()
	if rec == nil || rec.ClientID == "" || rec.Issuer != e.authSrv.issuer || rec.Token == nil || rec.Token.RefreshToken == "" {
		t.Fatalf("凭据记录不完整: %+v", rec)
	}
	if rec.RedirectURI != fmt.Sprintf("http://127.0.0.1:%d/callback", e.port) {
		t.Fatalf("回调地址错误: %s", rec.RedirectURI)
	}
	fi, err := os.Stat(e.credPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("凭据文件权限应为 600: %v %v", fi.Mode().Perm(), err)
	}
	di, _ := os.Stat(filepath.Dir(e.credPath))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("凭据目录权限应为 700: %v", di.Mode().Perm())
	}
	e.authSrv.mu.Lock()
	secrets := append([]string(nil), e.authSrv.seenSecr...)
	e.authSrv.mu.Unlock()
	for _, s := range secrets {
		if strings.Contains(logs.String(), s) {
			t.Fatal("日志中出现了 Token")
		}
	}
}

func TestOAuthRestartReusesCredentials(t *testing.T) {
	e := newOAuthEnv(t)
	mustConnected(t, e.start())
	m2 := e.start()
	mustConnected(t, m2)
	if e.authSrv.registers.Load() != 1 || e.opened.Load() != 1 {
		t.Fatalf("重启后不应重新注册或打开浏览器: register=%d open=%d", e.authSrv.registers.Load(), e.opened.Load())
	}
}

func TestOAuthRefreshPersistsNewToken(t *testing.T) {
	e := newOAuthEnv(t)
	e.authSrv.tokenTTL.Store(1)
	mustConnected(t, e.start())
	first := e.record().Token.AccessToken

	time.Sleep(1100 * time.Millisecond)
	e.authSrv.tokenTTL.Store(3600)
	m2 := e.start()
	mustConnected(t, m2)
	if e.authSrv.refreshes.Load() == 0 {
		t.Fatal("过期 Token 应通过 refresh token 刷新")
	}
	if e.opened.Load() != 1 || e.authSrv.registers.Load() != 1 {
		t.Fatal("刷新不应打开浏览器或重新注册")
	}
	if got := e.record().Token.AccessToken; got == first {
		t.Fatal("刷新后的 Token 应写回文件")
	}
}

func TestOAuthRefreshTokenRevokedReauthorizes(t *testing.T) {
	e := newOAuthEnv(t)
	mustConnected(t, e.start())
	e.authSrv.revokeAll()
	m2 := e.start()
	mustConnected(t, m2)
	if e.opened.Load() != 2 {
		t.Fatalf("refresh token 失效应重新打开浏览器: open=%d", e.opened.Load())
	}
	if e.authSrv.registers.Load() != 1 {
		t.Fatal("refresh token 失效不应重新注册")
	}
}

func TestOAuthInvalidClientReregisters(t *testing.T) {
	e := newOAuthEnv(t)
	mustConnected(t, e.start())
	oldClient := e.record().ClientID
	e.authSrv.forgetClients()
	m2 := e.start()
	mustConnected(t, m2)
	if e.authSrv.registers.Load() != 2 {
		t.Fatalf("已保存客户端失效应重新注册一次: register=%d", e.authSrv.registers.Load())
	}
	if e.record().ClientID == oldClient {
		t.Fatal("应保存新注册的客户端")
	}
}

func TestOAuthIssuerMismatchRejected(t *testing.T) {
	e := newOAuthEnv(t)
	mustConnected(t, e.start())
	store := &credentialStore{path: e.credPath}
	rec := e.record()
	rec.Issuer = "https://evil.example.com"
	rec.Token = nil // 强制走 401 -> 发现授权服务器
	if err := store.put(e.mcpURL, rec); err != nil {
		t.Fatal(err)
	}
	m2 := e.start()
	err := resultErr(m2)
	if err == nil || !strings.Contains(err.Error(), "mcp-oauth.json") {
		t.Fatalf("Issuer 不一致应连接失败并给出修复提示: %v", err)
	}
	if e.opened.Load() != 1 {
		t.Fatal("Issuer 不一致时不应打开浏览器")
	}
}

func TestOAuthFailureModes(t *testing.T) {
	t.Run("不支持DCR", func(t *testing.T) {
		e := newOAuthEnv(t)
		e.authSrv.noDCR.Store(true)
		err := resultErr(e.start())
		if err == nil || !strings.Contains(err.Error(), "DCR") {
			t.Fatalf("应提示不支持 DCR: %v", err)
		}
	})
	t.Run("用户拒绝授权", func(t *testing.T) {
		e := newOAuthEnv(t)
		e.authSrv.denyAuth.Store(true)
		err := resultErr(e.start())
		if err == nil || !strings.Contains(err.Error(), "access_denied") {
			t.Fatalf("回调 error 参数应透传: %v", err)
		}
	})
	t.Run("授权超时", func(t *testing.T) {
		e := newOAuthEnv(t)
		sc := e.serverConfig()
		sc.OAuth.AuthTimeoutSeconds = 1
		m, _ := NewManager(&Config{Servers: map[string]ServerConfig{"secure": sc}}, Options{
			CredentialPath: e.credPath,
			OpenBrowser:    func(string) error { return nil }, // 用户一直不操作
		})
		t.Cleanup(func() { _ = m.Close() })
		m.Connect(context.Background())
		if err := resultErr(m); err == nil {
			t.Fatal("授权超时应连接失败")
		}
	})
	t.Run("回调端口被占用", func(t *testing.T) {
		e := newOAuthEnv(t)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", e.port))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		err = resultErr(e.start())
		if err == nil || !strings.Contains(err.Error(), "redirect_port") {
			t.Fatalf("应提示修改 redirect_port: %v", err)
		}
	})
}

func TestOAuthBrowserAuthorizationSerialized(t *testing.T) {
	a := newFakeAuthServer(t)
	port := freePort(t)
	servers := map[string]ServerConfig{}
	for _, name := range []string{"one", "two", "three"} {
		servers[name] = ServerConfig{
			Transport: TransportStatelessHTTP,
			URL:       startProtectedMCPServer(t, a),
			OAuth:     &OAuthConfig{RedirectPort: port, AuthTimeoutSeconds: 20},
		}
	}
	var active, maxActive atomic.Int32
	m, _ := NewManager(&Config{Servers: servers}, Options{
		CredentialPath: filepath.Join(t.TempDir(), "mcp-oauth.json"),
		OpenBrowser: func(u string) error {
			n := active.Add(1)
			for {
				old := maxActive.Load()
				if n <= old || maxActive.CompareAndSwap(old, n) {
					break
				}
			}
			go func() {
				time.Sleep(100 * time.Millisecond) // 用户在浏览器停留
				// 在回调请求发出前结束计数：信号量在回调到达后即释放，
				// 若等 http.Get 返回再减，会把下一个授权的开始误算为并发。
				active.Add(-1)
				resp, err := http.Get(u)
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		},
	})
	t.Cleanup(func() { _ = m.Close() })
	m.Connect(context.Background())
	if err := resultErr(m); err != nil {
		t.Fatal(err)
	}
	if maxActive.Load() != 1 {
		t.Fatalf("浏览器授权应串行，最大并发 %d", maxActive.Load())
	}
	if len(m.Tools()) == 0 {
		t.Fatal("三个 Server 都应注册工具")
	}
}

func TestOAuthNoBrowserAfterStartup(t *testing.T) {
	e := newOAuthEnv(t)
	m := e.start()
	mustConnected(t, m)
	e.authSrv.revokeAll()

	_, err := findTool(t, m, "mcp__secure__echo").Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "重启") {
		t.Fatalf("运行中授权失效应提示重启: %v", err)
	}
	if e.opened.Load() != 1 || e.authSrv.registers.Load() != 1 {
		t.Fatalf("运行中不应打开浏览器或重新注册: open=%d register=%d", e.opened.Load(), e.authSrv.registers.Load())
	}
}

func TestCredentialStoreWarnsOnLoosePermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mcp-oauth.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	if _, err := (&credentialStore{path: p}).get("x"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "建议改为 600") {
		t.Fatal("宽松权限应产生警告")
	}
}
