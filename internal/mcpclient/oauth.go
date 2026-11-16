// internal/mcpclient/oauth.go
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

var errAuthAfterStartup = errors.New("授权已失效，请重启 claw 重新授权")

// oauthRecord 是一个 MCP Server 的持久化 OAuth 凭据
type oauthRecord struct {
	Issuer       string        `json:"issuer"`
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret,omitempty"`
	RedirectURI  string        `json:"redirect_uri"`
	AuthURL      string        `json:"auth_url"`
	TokenURL     string        `json:"token_url"`
	AuthStyle    int           `json:"auth_style"`
	Token        *oauth2.Token `json:"token"`
}

func (r *oauthRecord) oauth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     r.ClientID,
		ClientSecret: r.ClientSecret,
		RedirectURL:  r.RedirectURI,
		Endpoint: oauth2.Endpoint{
			AuthURL:   r.AuthURL,
			TokenURL:  r.TokenURL,
			AuthStyle: oauth2.AuthStyle(r.AuthStyle),
		},
	}
}

// newOAuthHandler 首次连接走 DCR；已有记录时作为预注册客户端恢复，并注入已保存的 Token
func (m *Manager) newOAuthHandler(sc ServerConfig) (auth.OAuthHandler, error) {
	key := sc.URL
	rec, err := m.store.get(key)
	if err != nil {
		return nil, err
	}
	if rec != nil && rec.RedirectURI != sc.redirectURL() {
		// 回调地址变更后已注册客户端不再可用
		log.Printf("[MCP] %s 的 OAuth 回调地址已变更，重新注册客户端\n", key)
		if err := m.store.delete(key); err != nil {
			return nil, err
		}
		rec = nil
	}

	sniffer := &issuerSniffer{base: http.DefaultTransport}
	hc := &http.Client{Transport: sniffer}
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RequestRefreshToken: true,
		Client:              hc,
	}
	prevIssuer := ""
	if rec != nil {
		prevIssuer = rec.Issuer
	}
	cfg.NewTokenSource = func(ctx context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		issuer := sniffer.get()
		if issuer == "" {
			issuer = prevIssuer
		}
		r := &oauthRecord{
			Issuer:       issuer,
			ClientID:     oc.ClientID,
			ClientSecret: oc.ClientSecret,
			RedirectURI:  oc.RedirectURL,
			AuthURL:      oc.Endpoint.AuthURL,
			TokenURL:     oc.Endpoint.TokenURL,
			AuthStyle:    int(oc.Endpoint.AuthStyle),
			Token:        tok,
		}
		if err := m.store.put(key, r); err != nil {
			log.Printf("[MCP] ⚠️ 保存 OAuth 凭据失败: %v\n", err)
		}
		return m.persisting(key, r, oc.TokenSource(ctx, tok)), nil
	}

	if rec != nil {
		cfg.RedirectURL = rec.RedirectURI
		creds := &oauthex.ClientCredentials{ClientID: rec.ClientID, Issuer: rec.Issuer}
		if rec.ClientSecret != "" {
			creds.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: rec.ClientSecret}
		}
		cfg.PreregisteredClient = creds
		// 已过期且无法刷新的 Token 不注入，直接走 401 -> 重新授权
		if rec.Token != nil && (rec.Token.Valid() || rec.Token.RefreshToken != "") {
			refreshCtx := context.WithValue(context.Background(), oauth2.HTTPClient, hc)
			cfg.InitialTokenSource = m.persisting(key, rec, rec.oauth2Config().TokenSource(refreshCtx, rec.Token))
		}
	} else {
		cfg.RedirectURL = sc.redirectURL()
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:              "my-tiny-claw",
				RedirectURIs:            []string{cfg.RedirectURL},
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
				TokenEndpointAuthMethod: "none",
			},
		}
	}
	cfg.AuthorizationCodeFetcher = m.codeFetcher(cfg.RedirectURL)
	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return nil, err
	}
	return &startupOnlyHandler{AuthorizationCodeHandler: h, m: m}, nil
}

// startupOnlyHandler 在 Connect 结束后拒绝任何授权流程（包括 DCR），避免运行中弹出浏览器或重复注册
type startupOnlyHandler struct {
	*auth.AuthorizationCodeHandler
	m *Manager
}

func (h *startupOnlyHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if h.m.connected.Load() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return errAuthAfterStartup
	}
	return h.AuthorizationCodeHandler.Authorize(ctx, req, resp)
}

// persisting 包装 TokenSource，Token 刷新后写回凭据文件
func (m *Manager) persisting(key string, rec *oauthRecord, base oauth2.TokenSource) oauth2.TokenSource {
	last := ""
	if rec.Token != nil {
		last = rec.Token.AccessToken
	}
	return &persistingTokenSource{base: base, last: last, save: func(t *oauth2.Token) {
		r := *rec
		r.Token = t
		if err := m.store.put(key, &r); err != nil {
			log.Printf("[MCP] ⚠️ 保存刷新后的 OAuth Token 失败: %v\n", err)
		}
	}}
}

type persistingTokenSource struct {
	base oauth2.TokenSource
	mu   sync.Mutex
	last string
	save func(*oauth2.Token)
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	t, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.AccessToken != p.last {
		p.last = t.AccessToken
		p.save(t)
	}
	return t, nil
}

// codeFetcher 在本地回环地址接收授权回调，全局串行。启动后的拦截由 startupOnlyHandler 负责。
func (m *Manager) codeFetcher(redirectURL string) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		select {
		case m.authSem <- struct{}{}:
			defer func() { <-m.authSem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		u, err := url.Parse(redirectURL)
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", u.Host)
		if err != nil {
			return nil, fmt.Errorf("无法监听 OAuth 回调地址 %s (端口可能被占用，可修改 oauth.redirect_port，修改后会重新注册): %w", u.Host, err)
		}
		type result struct {
			res *auth.AuthorizationResult
			err error
		}
		ch := make(chan result, 1)
		mux := http.NewServeMux()
		mux.HandleFunc(u.Path, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			var res result
			if e := q.Get("error"); e != "" {
				res.err = fmt.Errorf("授权服务器返回错误: %s %s", e, q.Get("error_description"))
				fmt.Fprintln(w, "授权失败，请回到终端查看错误。")
			} else {
				res.res = &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}
				fmt.Fprintln(w, "授权完成，可以关闭此页面。")
			}
			select {
			case ch <- res:
			default:
			}
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()

		fmt.Printf("\n🔐 MCP Server 需要授权，请在浏览器中完成登录:\n%s\n\n", args.URL)
		if err := m.openBrowser(args.URL); err != nil {
			log.Printf("[MCP] 无法自动打开浏览器，请手动复制上面的地址: %v\n", err)
		}

		select {
		case r := <-ch:
			return r.res, r.err
		case <-ctx.Done():
			return nil, fmt.Errorf("等待用户授权超时: %w", ctx.Err())
		}
	}
}

func openSystemBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

func isInvalidClient(err error) bool {
	if err == nil {
		return false
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return re.ErrorCode == "invalid_client"
	}
	return strings.Contains(err.Error(), "invalid_client")
}

// issuerSniffer 从授权服务器元数据响应中记录 issuer，用于恢复时绑定授权服务器。
// SDK 不向 NewTokenSource 暴露 issuer，这是拿到它的唯一途径。
type issuerSniffer struct {
	base   http.RoundTripper
	mu     sync.Mutex
	issuer string
}

func (s *issuerSniffer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	p := req.URL.Path
	if !strings.Contains(p, "/.well-known/oauth-authorization-server") && !strings.Contains(p, "/.well-known/openid-configuration") {
		return resp, nil
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if rerr != nil {
		return resp, nil
	}
	var meta struct {
		Issuer string `json:"issuer"`
	}
	if json.Unmarshal(body, &meta) == nil && meta.Issuer != "" {
		s.mu.Lock()
		s.issuer = meta.Issuer
		s.mu.Unlock()
	}
	return resp, nil
}

func (s *issuerSniffer) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issuer
}

// credentialStore 以 Server URL 为 Key 保存 OAuth 凭据，文件权限 0600
type credentialStore struct {
	path string
	mu   sync.Mutex
}

func defaultCredentialPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claw", "mcp-oauth.json"), nil
}

func (s *credentialStore) load() (map[string]*oauthRecord, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*oauthRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 OAuth 凭据失败: %w", err)
	}
	if fi, err := os.Stat(s.path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Printf("[MCP] ⚠️ OAuth 凭据文件 %s 权限为 %o，建议改为 600\n", s.path, fi.Mode().Perm())
	}
	recs := map[string]*oauthRecord{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("解析 OAuth 凭据 %s 失败: %w", s.path, err)
	}
	return recs, nil
}

func (s *credentialStore) get(key string) (*oauthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.load()
	if err != nil {
		return nil, err
	}
	return recs[key], nil
}

func (s *credentialStore) put(key string, rec *oauthRecord) error {
	return s.update(func(recs map[string]*oauthRecord) { recs[key] = rec })
}

func (s *credentialStore) delete(key string) error {
	return s.update(func(recs map[string]*oauthRecord) { delete(recs, key) })
}

func (s *credentialStore) update(fn func(map[string]*oauthRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.load()
	if err != nil {
		return err
	}
	fn(recs)
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mcp-oauth-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
