package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func startProxy(t *testing.T, rules Rules) *Server {
	t.Helper()
	s := &Server{Rules: rules}
	if _, err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestHTTPProxyPreservesResponseAndRemovesHopHeaders(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hop") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy leaked hop header")
		}
		w.Header().Set("Connection", "X-Response-Hop")
		w.Header().Set("X-Response-Hop", "secret")
		w.Header().Set("X-End-To-End", "kept")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("response-body"))
	}))
	defer origin.Close()
	s := startProxy(t, Rules{"127.0.0.1": RuleAllow})
	proxyURL, _ := url.Parse("http://" + s.HTTPAddr)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", origin.URL+"/path?q=1", nil)
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "secret")
	req.Header.Set("Proxy-Authorization", "private")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 201 || string(body) != "response-body" || response.Header.Get("X-Response-Hop") != "" || response.Header.Get("X-End-To-End") != "kept" {
		t.Fatalf("bad response: %d %q %v", response.StatusCode, body, response.Header)
	}
}
func TestHTTPSConnectProxy(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("tls-ok")) }))
	defer origin.Close()
	s := startProxy(t, Rules{"127.0.0.1": RuleAllow})
	u, _ := url.Parse("http://" + s.HTTPAddr)
	tr := &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	response, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "tls-ok" {
		t.Fatalf("body=%s", body)
	}
}
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln
}
func TestConnectPreservesBufferedTunnelBytes(t *testing.T) {
	echo := echoServer(t)
	s := startProxy(t, Rules{"127.0.0.1": RuleAllow})
	conn, err := net.Dial("tcp", s.HTTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(conn, "CONNECT "+echo.Addr().String()+" HTTP/1.1\r\nHost: "+echo.Addr().String()+"\r\n\r\nping")
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	data := make([]byte, 4)
	if _, err = io.ReadFull(reader, data); err != nil || string(data) != "ping" {
		t.Fatalf("tunnel=%q err=%v", data, err)
	}
}
func TestSOCKS5ConnectAndProtocolRejection(t *testing.T) {
	echo := echoServer(t)
	s := startProxy(t, Rules{"127.0.0.1": RuleAllow})
	t.Run("connect", func(t *testing.T) {
		c, err := net.Dial("tcp", s.SOCKSAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = c.Write([]byte{5, 1, 0})
		reply := make([]byte, 2)
		if _, err := io.ReadFull(c, reply); err != nil || !bytes.Equal(reply, []byte{5, 0}) {
			t.Fatalf("negotiation: %v %v", reply, err)
		}
		frame := []byte{5, 1, 0, 3, 9}
		frame = append(frame, []byte("127.0.0.1")...)
		frame = binary.BigEndian.AppendUint16(frame, uint16(echo.Addr().(*net.TCPAddr).Port))
		frame = append(frame, []byte("ping")...)
		_, _ = c.Write(frame)
		result := make([]byte, 14)
		if _, err := io.ReadFull(c, result); err != nil || result[1] != 0 || string(result[10:]) != "ping" {
			t.Fatalf("connect result=%v err=%v", result, err)
		}
	})
	for _, test := range []struct {
		name    string
		methods []byte
		cmd     byte
		want    byte
	}{{"auth", []byte{5, 1, 2}, 0, 255}, {"udp", []byte{5, 1, 0}, 3, 7}, {"bind", []byte{5, 1, 0}, 2, 7}} {
		t.Run(test.name, func(t *testing.T) {
			c, err := net.Dial("tcp", s.SOCKSAddr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = c.Write(test.methods)
			reply := make([]byte, 2)
			_, _ = io.ReadFull(c, reply)
			if test.cmd == 0 {
				if reply[1] != test.want {
					t.Fatal(reply)
				}
				return
			}
			_, _ = c.Write([]byte{5, test.cmd, 0, 1})
			result := make([]byte, 10)
			_, _ = io.ReadFull(c, result)
			if result[1] != test.want {
				t.Fatal(result)
			}
		})
	}
}
func TestProxyRulesRejectBeforeDNSAndRequireExplicitLocalAllow(t *testing.T) {
	calls := 0
	s := &Server{Rules: Rules{"deny.test": RuleDeny}, Approve: func(context.Context, string, int) (Decision, error) { return Allow, nil }, Resolve: func(context.Context, string) ([]net.IP, error) { calls++; return []net.IP{net.ParseIP("8.8.8.8")}, nil }}
	if _, _, err := s.authorize(context.Background(), "deny.test", 443); err == nil || calls != 0 {
		t.Fatal("deny resolved")
	}
	if _, _, err := s.authorize(context.Background(), "127.0.0.1", 443); err == nil {
		t.Fatal("approval alone allowed loopback")
	}
	s.Rules = Rules{"api.test": RuleAllow}
	s.Resolve = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}, nil
	}
	if _, _, err := s.authorize(context.Background(), "api.test", 443); err == nil {
		t.Fatal("mixed private DNS reply accepted")
	}
}
func TestNormalizeRulesAndInvalidHosts(t *testing.T) {
	rules, err := NormalizeRules(Rules{"EXAMPLE.COM.": RuleAllow, "example.com": RuleDeny})
	if err != nil || rules["example.com"] != RuleDeny {
		t.Fatalf("%v %v", rules, err)
	}
	for _, raw := range []string{"a..com", "*.com", "abc:443", "-a.com", "a_.com", "a.com\x00"} {
		if _, _, err := NormalizeHost(raw); err == nil {
			t.Errorf("invalid host accepted: %q", raw)
		}
	}
	if _, err := NormalizeRules(Rules{"*.*.com": RuleAllow}); err == nil {
		t.Fatal("invalid wildcard")
	}
}
func TestHTTPProxyDenyResponse(t *testing.T) {
	s := startProxy(t, Rules{"example.test": RuleDeny})
	u, _ := url.Parse("http://" + s.HTTPAddr)
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	resp, err := client.Get("http://example.test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 || !strings.Contains(string(body), "拒绝") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}
