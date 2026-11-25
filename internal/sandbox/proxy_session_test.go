package sandbox

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

func TestProxyPortGroupsAreIsolatedAndSharedByNormalizedRules(t *testing.T) {
	p := &Profile{Name: "one", Network: NetworkProxy, Domains: map[string]DomainRule{"EXAMPLE.COM.": DomainDeny}}
	s, err := NewSession(p, ApprovalNever, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, first, err := s.proxyForProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	same := cloneProfile(p)
	same.Name = "two"
	same.Domains = map[string]DomainRule{"example.com": DomainDeny}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Go(func() {
			_, ports, err := s.proxyForProfile(same)
			if err != nil || ports != first {
				t.Errorf("same rules: ports=%+v err=%v", ports, err)
			}
		})
	}
	wg.Wait()
	different := cloneProfile(same)
	different.Domains = map[string]DomainRule{"example.com": DomainAllow}
	_, other, err := s.proxyForProfile(different)
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("different rules share ports")
	}
	if len(s.proxyServers) != 2 {
		t.Fatal("duplicate groups leaked")
	}
	u, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(first.HTTPPort))
	transport := &http.Transport{Proxy: http.ProxyURL(u)}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(context.Background(), "GET", "http://example.com", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != 403 {
		t.Fatalf("deny group returned %d", response.StatusCode)
	}
}
