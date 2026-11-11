package outbound

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboundTransportUsesEnvironmentProxyAndHonorsNoProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://172.24.176.1:10808")
	t.Setenv("HTTPS_PROXY", "http://172.24.176.1:10808")
	t.Setenv("NO_PROXY", ".se7endot.top,100.64.0.0/10")
	transport := newOutboundTransport(resolveOutboundHost)

	proxiedRequest, _ := http.NewRequest(http.MethodGet, "https://api.mikoto.vip/v1/models", nil)
	proxyURL, err := transport.Proxy(proxiedRequest)
	if err != nil {
		t.Fatal(err)
	}
	wantProxy, _ := url.Parse("http://172.24.176.1:10808")
	if proxyURL == nil || proxyURL.String() != wantProxy.String() {
		t.Fatalf("proxy = %v, want %v", proxyURL, wantProxy)
	}

	directRequest, _ := http.NewRequest(http.MethodGet, "https://api.se7endot.top/v1/models", nil)
	proxyURL, err = transport.Proxy(directRequest)
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL != nil {
		t.Fatalf("NO_PROXY request proxy = %v, want nil", proxyURL)
	}
}

func TestConfiguredProxyHostIsTrustedAsDeploymentEgress(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://172.24.176.1:10808")
	if !configuredProxyHost("172.24.176.1") {
		t.Fatal("configuredProxyHost() rejected the configured private proxy")
	}
	if configuredProxyHost("172.24.176.2") {
		t.Fatal("configuredProxyHost() accepted an unrelated private host")
	}
}

func TestOutboundHTTPClientWithProxyFailsClosedForInvalidURL(t *testing.T) {
	client := OutboundHTTPClientWithProxy(time.Second, "ftp://127.0.0.1:1080")
	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "渠道代理地址无效") {
		t.Fatalf("invalid proxy should fail closed, got %v", err)
	}
}

func TestOutboundHTTPClientWithProxyFallsBackForEmptyURL(t *testing.T) {
	client := OutboundHTTPClientWithProxy(time.Second, " \t")
	if client.Transport != outboundTransport {
		t.Fatal("empty proxy URL should use the global outbound transport")
	}
}

func TestNormalizeOutboundHeadersAllowsCustomUserAgent(t *testing.T) {
	headers, err := NormalizeOutboundHeaders([]OutboundHeader{{Name: "user-agent", Value: "Custom Gateway/2.0"}, {Name: "X-Gateway-Tenant", Value: "tenant-a"}})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	ApplyOutboundHeaders(request, headers)
	ApplyDefaultOutboundHeaders(request)
	if got := request.Header.Get("User-Agent"); got != "Custom Gateway/2.0" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := request.Header.Get("X-Gateway-Tenant"); got != "tenant-a" {
		t.Fatalf("X-Gateway-Tenant = %q", got)
	}
}

func TestNormalizeOutboundHeadersRejectsUnsafeAndDuplicateValues(t *testing.T) {
	tests := [][]OutboundHeader{
		{{Name: "Authorization", Value: "Bearer attacker"}},
		{{Name: "X-Test", Value: "safe\r\ninjected: true"}},
		{{Name: "X-Test", Value: "one"}, {Name: "x-test", Value: "two"}},
	}
	for _, headers := range tests {
		if _, err := NormalizeOutboundHeaders(headers); err == nil {
			t.Fatalf("NormalizeOutboundHeaders(%#v) should fail", headers)
		}
	}
}

func TestDecodeRelayOutboundHeaders(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte(`[{"name":"User-Agent","value":"Relay Agent"}]`))
	headers, err := DecodeRelayOutboundHeaders(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || headers[0].Name != "User-Agent" || headers[0].Value != "Relay Agent" {
		t.Fatalf("DecodeRelayOutboundHeaders() = %#v", headers)
	}
}

func TestApplyDefaultOutboundHeaders(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	ApplyDefaultOutboundHeaders(request)
	if got := request.Header.Get("User-Agent"); got != DefaultOutboundUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, DefaultOutboundUserAgent)
	}

	request.Header.Set("User-Agent", "custom-agent")
	ApplyDefaultOutboundHeaders(request)
	if got := request.Header.Get("User-Agent"); got != "custom-agent" {
		t.Fatalf("custom User-Agent = %q", got)
	}
}

func TestValidateOutboundURLRejectsPrivateHosts(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "false")
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "")
	for _, rawURL := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://169.254.169.254/latest/meta-data"} {
		if _, err := ValidateOutboundURL(rawURL); err == nil {
			t.Fatalf("ValidateOutboundURL(%q) should fail", rawURL)
		}
	}
}

func TestValidateOutboundURLAllowsExplicitPrivateUpstreamOverride(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "")
	if _, err := ValidateOutboundURL("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("ValidateOutboundURL() error = %v", err)
	}
}

func TestValidateOutboundURLAllowsOnlyNamedPrivateUpstream(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "false")
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
	if _, err := ValidateOutboundURL("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("ValidateOutboundURL() error = %v", err)
	}
	if _, err := ValidateOutboundURL("http://127.0.0.2:8080"); err == nil {
		t.Fatal("ValidateOutboundURL() should reject an unlisted private host")
	}
}

func TestAllowedPrivateUpstreamHostUsesExactCaseInsensitiveMatch(t *testing.T) {
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", " API.EXAMPLE.COM.,trusted.internal ")
	if !AllowedPrivateUpstreamHost("api.example.com") {
		t.Fatal("AllowedPrivateUpstreamHost() should allow exact normalized hostname")
	}
	if AllowedPrivateUpstreamHost("api.example.com.evil.test") {
		t.Fatal("AllowedPrivateUpstreamHost() should reject hostname suffix confusion")
	}
}

func TestValidateCustomRelayURLAllowsHTTPOnlyForExactPrivateAllowlist(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "")
	if _, err := ValidateCustomRelayURL("http://127.0.0.1:8080/v1/models"); err == nil {
		t.Fatal("ValidateCustomRelayURL() should reject HTTP without an exact host allowlist")
	}
	if _, err := ValidateCustomRelayURL("https://127.0.0.1:8080/v1/models"); err == nil {
		t.Fatal("ValidateCustomRelayURL() should ignore the global private upstream override")
	}
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
	for _, rawURL := range []string{
		"http://127.0.0.1:8080/v1/models",
		"https://127.0.0.1:8080/v1/models",
	} {
		if _, err := ValidateCustomRelayURL(rawURL); err != nil {
			t.Fatalf("ValidateCustomRelayURL(%q) error = %v", rawURL, err)
		}
	}
}

func TestValidateCustomRelayURLRejectsCredentialsAndFragment(t *testing.T) {
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
	for _, rawURL := range []string{
		"https://user:pass@127.0.0.1/v1/models",
		"https://127.0.0.1/v1/models#secret",
	} {
		if _, err := ValidateCustomRelayURL(rawURL); err == nil {
			t.Fatalf("ValidateCustomRelayURL(%q) should fail", rawURL)
		}
	}
}

func TestBlockedCustomRelayIPRejectsCarrierGradeNATAndReservedRanges(t *testing.T) {
	for _, value := range []string{"100.100.100.200", "192.0.2.10", "198.18.0.1", "2001:db8::1"} {
		if !blockedCustomRelayIP(net.ParseIP(value)) {
			t.Fatalf("blockedCustomRelayIP(%q) = false", value)
		}
	}
	if blockedCustomRelayIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("blockedCustomRelayIP() rejected a public address")
	}
}

func TestCustomRelayHTTPClientDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected = true
	}))
	defer destination.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer source.Close()

	client := CustomRelayHTTPClient(time.Second)
	client.Transport = source.Client().Transport
	if _, err := client.Get(source.URL); err == nil {
		t.Fatal("CustomRelayHTTPClient() should reject redirects")
	}
	if redirected {
		t.Fatal("redirect destination should not receive the request")
	}
}

func TestOutboundHTTPClientWithProxyReusesTransportPerProxy(t *testing.T) {
	c1 := OutboundHTTPClientWithProxy(10*time.Second, "http://127.0.0.1:8888")
	c2 := OutboundHTTPClientWithProxy(30*time.Second, "  HTTP://127.0.0.1:8888  ")
	c3 := OutboundHTTPClientWithProxy(10*time.Second, "http://127.0.0.1:8889")

	if c1.Timeout != 10*time.Second || c2.Timeout != 30*time.Second {
		t.Fatalf("timeouts = (%v, %v), want (10s, 30s)", c1.Timeout, c2.Timeout)
	}
	if _, ok := c1.Transport.(*http.Transport); !ok {
		t.Fatalf("c1.Transport = %T, want *http.Transport", c1.Transport)
	}
	if c1.Transport != c2.Transport {
		t.Fatal("expected normalized identical proxyURL with different timeouts to share *http.Transport")
	}
	if c1.Transport == c3.Transport {
		t.Fatal("expected distinct proxy ports to use distinct *http.Transport instances")
	}

	hostUpper := OutboundHTTPClientWithProxy(5*time.Second, "HTTP://PROXY.EXAMPLE.COM:8080")
	hostLower := OutboundHTTPClientWithProxy(5*time.Second, "http://proxy.example.com:8080")
	if hostUpper.Transport != hostLower.Transport {
		t.Fatal("expected scheme/host case differences to share *http.Transport")
	}

	credUpper := OutboundHTTPClientWithProxy(5*time.Second, "http://u:PassA@127.0.0.1:8888")
	credLower := OutboundHTTPClientWithProxy(5*time.Second, "http://u:passa@127.0.0.1:8888")
	credUser2 := OutboundHTTPClientWithProxy(5*time.Second, "http://u2:PassA@127.0.0.1:8888")
	if credUpper.Transport == credLower.Transport {
		t.Fatal("expected case-sensitive password difference to isolate *http.Transport")
	}
	if credUpper.Transport == credUser2.Transport {
		t.Fatal("expected distinct usernames to isolate *http.Transport")
	}
	if credUpper.Transport == c1.Transport {
		t.Fatal("expected authenticated and unauthenticated proxy URLs to isolate *http.Transport")
	}

	socksClient := OutboundHTTPClientWithProxy(5*time.Second, "SOCKS5://127.0.0.1:8888")
	socksClient2 := OutboundHTTPClientWithProxy(15*time.Second, "socks5://127.0.0.1:8888")
	if _, ok := socksClient.Transport.(*http.Transport); !ok {
		t.Fatalf("socksClient.Transport = %T, want *http.Transport", socksClient.Transport)
	}
	if socksClient.Transport != socksClient2.Transport {
		t.Fatal("expected SOCKS5 scheme case difference to share *http.Transport")
	}
	if socksClient.Transport == c1.Transport {
		t.Fatal("expected socks5 and http proxy schemes to isolate *http.Transport")
	}

	invalid := OutboundHTTPClientWithProxy(5*time.Second, "ftp://127.0.0.1:1080")
	if _, ok := invalid.Transport.(rejectingTransport); !ok {
		t.Fatalf("invalid proxy Transport = %T, want rejectingTransport", invalid.Transport)
	}
}

func TestProxyTransportPoolEvictsBoundedAndClosesIdle(t *testing.T) {
	var closedConns atomic.Int32
	proxyServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	proxyServer.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closedConns.Add(1)
		}
	}
	proxyServer.Start()
	defer proxyServer.Close()

	var evictedCount atomic.Int32
	pool := newProxyTransportPool(3)
	pool.onEvict = func(*http.Transport) {
		evictedCount.Add(1)
	}

	firstProxyURL := proxyServer.URL
	firstTransport := pool.getOrCreate(firstProxyURL)
	if firstTransport == nil {
		t.Fatal("expected valid *http.Transport for proxyServer.URL")
	}

	// Open a keep-alive HTTP connection through firstTransport so an idle socket sits in its pool.
	req, err := http.NewRequest(http.MethodGet, "http://example.com/ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: firstTransport, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_ = resp.Body.Close()

	// Insert 3 distinct proxy URLs so firstTransport is evicted from the capacity-3 pool.
	for i := 1; i <= 3; i++ {
		tr := pool.getOrCreate(fmt.Sprintf("http://127.0.0.1:%d", 19000+i))
		if tr == nil {
			t.Fatalf("expected non-nil transport for slot %d", i)
		}
	}
	if got := pool.len(); got != 3 {
		t.Fatalf("pool.len() = %d, want 3", got)
	}
	if got := evictedCount.Load(); got != 1 {
		t.Fatalf("evictedCount = %d, want 1", got)
	}

	// Eviction must call CloseIdleConnections() so the idle TCP socket closes promptly.
	deadline := time.Now().Add(time.Second)
	for closedConns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closedConns.Load() == 0 {
		t.Fatal("expected evicted transport to close its idle proxy TCP connection")
	}

	recreated := pool.getOrCreate(firstProxyURL)
	if recreated == firstTransport {
		t.Fatal("expected evicted proxy URL to allocate a fresh *http.Transport on re-insert")
	}

	// Concurrent stress test for bounded size and race safety.
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = pool.getOrCreate(fmt.Sprintf("http://127.0.0.1:%d", 20000+((worker+i)%12)))
				_ = pool.getOrCreate("ftp://invalid-scheme:1080")
			}
		}(g)
	}
	wg.Wait()
	if got := pool.len(); got > 3 {
		t.Fatalf("pool.len() after concurrent stress = %d, want <= 3", got)
	}
}

func TestDialValidatedAddressesFallsBackToHealthyIP(t *testing.T) {
	var accepted atomic.Int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()

	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closedLn.Addr().String()
	_ = closedLn.Close()

	dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialSequentialTargets(context.Background(), dialer, "tcp", []string{
		closedAddr,
		ln.Addr().String(),
	})
	if err != nil {
		t.Fatalf("dialSequentialTargets failed to fall back to healthy target: %v", err)
	}
	_ = conn.Close()

	// Wait briefly for Accept goroutine to observe the connection, then reset counter.
	deadline := time.Now().Add(500 * time.Millisecond)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	accepted.Store(0)

	// Cancelled ctx must abort immediately without dialing subsequent targets.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dialSequentialTargets(cancelledCtx, dialer, "tcp", []string{closedAddr, ln.Addr().String()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled dialSequentialTargets err = %v, want context.Canceled", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := accepted.Load(); got != 0 {
		t.Fatalf("cancelled context dialed healthy listener %d times, want 0", got)
	}

	// All-failing targets must return the first non-nil dial error.
	_, allFailErr := dialSequentialTargets(context.Background(), dialer, "tcp", []string{
		"invalid-address-missing-port",
		closedAddr,
	})
	if allFailErr == nil || !strings.Contains(allFailErr.Error(), "missing port") {
		t.Fatalf("all-failing dialSequentialTargets err = %v, want first error mentioning missing port", allFailErr)
	}
}

func TestValidatedRequestContextAvoidsDuplicateLookupAndPreservesRedirectSSRF(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "false")
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "upstream.example.test")

	var secretHit atomic.Bool
	secretServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secretHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer secretServer.Close()
	_, secretPort, err := net.SplitHostPort(secretServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect-loopback":
			http.Redirect(w, r, "http://127.0.0.1:"+secretPort+"/secret", http.StatusFound)
		case "/redirect-private-host":
			http.Redirect(w, r, "http://internal.corp.test:"+secretPort+"/secret", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstreamServer.Close()
	defer outboundTransport.CloseIdleConnections()

	_, upstreamPort, err := net.SplitHostPort(upstreamServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	lookupCounts := map[string]int{}
	prevLookup := lookupIPFunc
	lookupIPFunc = func(_ context.Context, _ string, host string) ([]net.IP, error) {
		normalized := normalizeOutboundHost(host)
		mu.Lock()
		lookupCounts[normalized]++
		mu.Unlock()
		switch normalized {
		case "upstream.example.test", "127.0.0.1", "internal.corp.test":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		case "cgnat.example.test":
			return []net.IP{net.ParseIP("100.64.0.1")}, nil
		default:
			return nil, errors.New("unexpected host: " + normalized)
		}
	}
	t.Cleanup(func() { lookupIPFunc = prevLookup })

	// (a) ValidateOutboundRequest + OutboundHTTPClient.Do invokes lookupIPFunc once instead of twice.
	req, err := http.NewRequest(http.MethodGet, "http://upstream.example.test:"+upstreamPort+"/ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err = ValidateOutboundRequest(req)
	if err != nil {
		t.Fatalf("ValidateOutboundRequest() error = %v", err)
	}
	resp, err := OutboundHTTPClient(5 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("OutboundHTTPClient.Do() error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mu.Lock()
	gotUpstreamLookups := lookupCounts["upstream.example.test"]
	mu.Unlock()
	if gotUpstreamLookups != 1 {
		t.Fatalf("lookupIPFunc count for upstream.example.test = %d, want 1", gotUpstreamLookups)
	}

	// (b) 302 redirects to disallowed loopback/private hosts remain blocked by CheckRedirect & DialContext.
	for _, path := range []string{"/redirect-loopback", "/redirect-private-host"} {
		redirectReq, reqErr := http.NewRequest(http.MethodGet, "http://upstream.example.test:"+upstreamPort+path, nil)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		redirectReq, reqErr = ValidateOutboundRequest(redirectReq)
		if reqErr != nil {
			t.Fatalf("ValidateOutboundRequest(%s) error = %v", path, reqErr)
		}
		if _, doErr := OutboundHTTPClient(5 * time.Second).Do(redirectReq); doErr == nil {
			t.Fatalf("redirect %s should be blocked by CheckRedirect SSRF validation", path)
		}
		// Even if CheckRedirect were permissive, DialContext must not reuse upstream.example.test's context entry for another host.
		permissiveClient := &http.Client{
			Transport: outboundTransport,
			Timeout:   5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return nil
			},
		}
		if _, doErr := permissiveClient.Do(redirectReq); doErr == nil {
			t.Fatalf("redirect %s should be blocked by DialContext SSRF validation", path)
		}
	}
	if secretHit.Load() {
		t.Fatal("disallowed private redirect target should never receive a request")
	}

	// (c) Context validated for standard outbound policy cannot bypass blockedCustomRelayIP (100.64.0.1 CGNAT) on CustomRelayHTTPClient.
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "")
	cgnatReq, err := http.NewRequest(http.MethodGet, "https://cgnat.example.test/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	cgnatReq, err = ValidateOutboundRequest(cgnatReq)
	if err != nil {
		t.Fatalf("ValidateOutboundRequest(cgnat) should succeed under outbound policy, got %v", err)
	}
	_, relayErr := CustomRelayHTTPClient(time.Second).Do(cgnatReq)
	if relayErr == nil || !strings.Contains(relayErr.Error(), "不允许访问保留地址或特殊用途地址") {
		t.Fatalf("CustomRelayHTTPClient.Do(cgnatReq) = %v, want CGNAT SSRF rejection", relayErr)
	}

	// (d) WithValidatedHostAddrs makes a defensive copy and DialContext still re-verifies blockedOutboundIP.
	rawIPs := []net.IP{net.ParseIP("93.184.216.34").To4()}
	safeCtx := WithValidatedHostAddrs(context.Background(), "defensive.example.test", rawIPs)
	rawIPs[0][0] = 127 // Mutate caller slice after registration; cached entry must remain 93.184.216.34.
	resolvedIPs, err := resolveOutboundHost(safeCtx, "defensive.example.test")
	if err != nil || len(resolvedIPs) != 1 || !resolvedIPs[0].Equal(net.IPv4(93, 184, 216, 34)) {
		t.Fatalf("defensive copy check failed: ips=%v, err=%v", resolvedIPs, err)
	}

	forgedPrivateCtx := WithValidatedHostAddrs(context.Background(), "forged.example.test", []net.IP{net.ParseIP("127.0.0.1")})
	if _, err := resolveOutboundHost(forgedPrivateCtx, "forged.example.test"); err == nil {
		t.Fatal("resolveOutboundHost must re-verify and reject private IPs even on context hit")
	}

	// (e) Repeated WithValidatedOutboundURL calls on the same ctx must preserve the original expiresAt deadline.
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "upstream.example.test")
	ctx1, _, err := WithValidatedOutboundURL(context.Background(), "http://upstream.example.test:"+upstreamPort+"/v1")
	if err != nil {
		t.Fatalf("first WithValidatedOutboundURL() error = %v", err)
	}
	entry1, ok := ctx1.Value(validatedHostContextKey{}).(*validatedHostContextEntry)
	if !ok || entry1 == nil {
		t.Fatal("expected validatedHostContextEntry on ctx1")
	}
	time.Sleep(15 * time.Millisecond)
	ctx2, _, err := WithValidatedOutboundURL(ctx1, "http://upstream.example.test:"+upstreamPort+"/v2")
	if err != nil {
		t.Fatalf("second WithValidatedOutboundURL() error = %v", err)
	}
	entry2, ok := ctx2.Value(validatedHostContextKey{}).(*validatedHostContextEntry)
	if !ok || entry2 == nil {
		t.Fatal("expected validatedHostContextEntry on ctx2")
	}
	if !entry2.expiresAt.Equal(entry1.expiresAt) || entry2 != entry1 {
		t.Fatalf("expected repeated WithValidatedOutboundURL to preserve entry & expiresAt (%v), got %v (samePtr=%v)", entry1.expiresAt, entry2.expiresAt, entry2 == entry1)
	}
}

func TestOutboundRejectsMalformedIPSlicesAndNeverDialsLocalhost(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "false")
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "")

	for _, malformed := range []net.IP{nil, {}, {1, 2, 3}, make(net.IP, 5)} {
		if !blockedOutboundIP(malformed) {
			t.Fatalf("blockedOutboundIP(%#v) = false, want true", malformed)
		}
	}

	var localhostHit atomic.Bool
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			localhostHit.Store(true)
			_ = c.Close()
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	prevLookup := lookupIPFunc
	lookupIPFunc = func(_ context.Context, _ string, host string) ([]net.IP, error) {
		return nil, errors.New("dns lookup failed for " + host)
	}
	t.Cleanup(func() { lookupIPFunc = prevLookup })

	// Forged empty/malformed net.IP slices in WithValidatedHostAddrs must not be cached or dial ":port" (localhost).
	forgedCtx := WithValidatedHostAddrs(context.Background(), "forged-empty.example.test", []net.IP{{}, {1, 2, 3}})
	if forgedCtx.Value(validatedHostContextKey{}) != nil {
		t.Fatal("WithValidatedHostAddrs must not store malformed/empty net.IP entries")
	}
	req, err := http.NewRequestWithContext(forgedCtx, http.MethodGet, "http://forged-empty.example.test:"+port+"/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OutboundHTTPClient(time.Second).Do(req); err == nil {
		t.Fatal("expected request with malformed IP slice context to fail")
	}

	// Even if a custom resolver returned malformed net.IP slices, newOutboundTransport must never dial ":port".
	malformedTransport := newOutboundTransport(func(context.Context, string) ([]net.IP, error) {
		return []net.IP{{}, {1, 2, 3}}, nil
	})
	if _, dialErr := malformedTransport.DialContext(context.Background(), "tcp", "forged-empty.example.test:"+port); dialErr == nil {
		t.Fatal("expected newOutboundTransport to reject malformed resolved IPs rather than dialing :port")
	}
	time.Sleep(20 * time.Millisecond)
	if localhostHit.Load() {
		t.Fatal("malformed net.IP caused outbound transport to dial localhost")
	}
}
