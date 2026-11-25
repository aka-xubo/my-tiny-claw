package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Approver func(context.Context, string, int) (Decision, error)
type Resolver func(context.Context, string) ([]net.IP, error)
type Endpoints struct{ HTTPPort, SOCKSPort int }
type Server struct {
	Rules               Rules
	Approve             Approver
	Resolve             Resolver
	HTTPAddr, SOCKSAddr string
	mu                  sync.Mutex
	httpLn, socksLn     net.Listener
	httpServer          *http.Server
	connections         map[net.Conn]struct{}
	ctx                 context.Context
	cancel              context.CancelFunc
	closed              bool
}

func (s *Server) Start() (Endpoints, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Endpoints{}, errors.New("代理已关闭")
	}
	if s.httpLn != nil {
		return Endpoints{s.httpLn.Addr().(*net.TCPAddr).Port, s.socksLn.Addr().(*net.TCPAddr).Port}, nil
	}
	rules, err := NormalizeRules(s.Rules)
	if err != nil {
		return Endpoints{}, err
	}
	s.Rules = rules
	if s.Resolve == nil {
		s.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	h, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Endpoints{}, err
	}
	k, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.Close()
		return Endpoints{}, err
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.connections = make(map[net.Conn]struct{})
	s.httpLn, s.socksLn = h, k
	s.HTTPAddr, s.SOCKSAddr = h.Addr().String(), k.Addr().String()
	s.httpServer = &http.Server{Handler: http.HandlerFunc(s.handleHTTP), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return s.ctx }}
	go func() { _ = s.httpServer.Serve(h) }()
	go s.serveSOCKS(k)
	return Endpoints{h.Addr().(*net.TCPAddr).Port, k.Addr().(*net.TCPAddr).Port}, nil
}
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.httpServer != nil {
		_ = s.httpServer.Close()
	}
	if s.httpLn != nil {
		_ = s.httpLn.Close()
	}
	if s.socksLn != nil {
		_ = s.socksLn.Close()
	}
	for c := range s.connections {
		_ = c.Close()
	}
	return nil
}
func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return false
	}
	if s.connections == nil {
		s.connections = make(map[net.Conn]struct{})
	}
	s.connections[c] = struct{}{}
	return true
}
func (s *Server) forget(c net.Conn) { s.mu.Lock(); delete(s.connections, c); s.mu.Unlock(); c.Close() }
func (s *Server) authorize(ctx context.Context, rawHost string, port int) (string, net.IP, error) {
	if port < 1 || port > 65535 {
		return "", nil, errors.New("端口非法")
	}
	host, ip, err := NormalizeHost(rawHost)
	if err != nil {
		return "", nil, err
	}
	match := Match(host, s.Rules)
	switch match {
	case Deny:
		return "", nil, fmt.Errorf("域名被拒绝: %s", host)
	case NoMatch:
		if s.Approve == nil {
			return "", nil, fmt.Errorf("未知域名未获准: %s", host)
		}
		decision, err := s.Approve(ctx, host, port)
		if err != nil {
			return "", nil, err
		}
		if decision != Allow {
			return "", nil, fmt.Errorf("域名未获批准: %s", host)
		}
	}
	explicitLocal := match == Allow && IsExplicitLocalHost(host)
	if ip == nil {
		resolver := s.Resolve
		if resolver == nil {
			resolver = func(ctx context.Context, h string) ([]net.IP, error) {
				return net.DefaultResolver.LookupIP(ctx, "ip", h)
			}
		}
		ips, err := resolver(ctx, host)
		if err != nil {
			return "", nil, fmt.Errorf("解析域名失败: %w", err)
		}
		if len(ips) == 0 {
			return "", nil, errors.New("DNS 未返回地址")
		}
		for _, candidate := range ips {
			if candidate == nil || IsPrivateOrLocal(candidate) && !explicitLocal {
				return "", nil, fmt.Errorf("域名解析到受限地址: %s", host)
			}
		}
		ip = ips[0]
	} else if IsPrivateOrLocal(ip) && !explicitLocal {
		return "", nil, fmt.Errorf("私有地址被拒绝: %s", host)
	}
	return host, ip, nil
}
func targetPort(raw string, defaultPort int) (int, error) {
	if raw == "" {
		return defaultPort, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("端口非法")
	}
	return port, nil
}
func (s *Server) handleHTTP(w http.ResponseWriter, req *http.Request) {
	host, port := req.URL.Hostname(), 80
	var err error
	if req.Method == http.MethodConnect {
		hostRaw, portRaw, parseErr := net.SplitHostPort(req.Host)
		if parseErr != nil {
			http.Error(w, "CONNECT 必须指定 host:port", 400)
			return
		}
		host = hostRaw
		port, err = targetPort(portRaw, 443)
	} else {
		if req.URL.Scheme != "http" || host == "" || req.URL.User != nil {
			http.Error(w, "需要 absolute-form HTTP 请求", 400)
			return
		}
		port, err = targetPort(req.URL.Port(), 80)
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	_, ip, err := s.authorize(req.Context(), host, port)
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	address := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	if req.Method == http.MethodConnect {
		upstream, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(req.Context(), "tcp", address)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		if !s.track(upstream) {
			return
		}
		defer s.forget(upstream)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "CONNECT 不受支持", 500)
			return
		}
		client, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		if !s.track(client) {
			return
		}
		defer s.forget(client)
		if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if rw.Flush() != nil {
			return
		}
		tunnel(client, rw.Reader, upstream)
		return
	}
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	forwarded := req.Clone(req.Context())
	forwarded.Close = false
	forwarded.RequestURI = ""
	forwarded.Host = forwarded.URL.Host
	stripHopHeaders(forwarded.Header)
	response, err := transport.RoundTrip(forwarded)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer response.Body.Close()
	stripHopHeaders(response.Header)
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}
func stripHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}
func tunnel(client net.Conn, reader io.Reader, upstream net.Conn) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, reader)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	_ = upstream.Close()
	<-done
}
func (s *Server) serveSOCKS(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if !s.track(conn) {
			return
		}
		go func() { defer s.forget(conn); s.handleSOCKS(conn) }()
	}
}
func (s *Server) handleSOCKS(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(75 * time.Second))
	reader := bufio.NewReader(conn)
	version, err := reader.ReadByte()
	if err != nil || version != 5 {
		return
	}
	count, err := reader.ReadByte()
	if err != nil {
		return
	}
	methods := make([]byte, int(count))
	if _, err = io.ReadFull(reader, methods); err != nil {
		return
	}
	noAuth := false
	for _, method := range methods {
		if method == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = conn.Write([]byte{5, 255})
		return
	}
	if _, err = conn.Write([]byte{5, 0}); err != nil {
		return
	}
	header := make([]byte, 4)
	if _, err = io.ReadFull(reader, header); err != nil {
		return
	}
	if header[0] != 5 || header[2] != 0 {
		socksReply(conn, 1)
		return
	}
	if header[1] != 1 {
		socksReply(conn, 7)
		return
	}
	host, err := readSOCKSHost(reader, header[3])
	if err != nil {
		socksReply(conn, 8)
		return
	}
	portBytes := make([]byte, 2)
	if _, err = io.ReadFull(reader, portBytes); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(portBytes))
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	_, ip, err := s.authorize(ctx, host, port)
	if err != nil {
		socksReply(conn, 2)
		return
	}
	upstream, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	if err != nil {
		socksReply(conn, 5)
		return
	}
	if !s.track(upstream) {
		return
	}
	defer s.forget(upstream)
	socksReply(conn, 0)
	_ = conn.SetDeadline(time.Time{})
	tunnel(conn, reader, upstream)
}
func readSOCKSHost(reader *bufio.Reader, addressType byte) (string, error) {
	size := 0
	switch addressType {
	case 1:
		size = 4
	case 4:
		size = 16
	case 3:
		n, err := reader.ReadByte()
		if err != nil || n == 0 {
			return "", errors.New("域名长度非法")
		}
		size = int(n)
	default:
		return "", errors.New("不支持的 SOCKS 地址类型")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", err
	}
	if addressType == 3 {
		return string(data), nil
	}
	return net.IP(data).String(), nil
}
func socksReply(conn net.Conn, code byte) { _, _ = conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}) }
