package outbound

import (
	"container/list"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"yingce/backend/internal/kernel"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/proxy"
)

const (
	maxOutboundRedirects              = 5
	maxOutboundHeaderCount            = 32
	maxOutboundHeaderBytes            = 16 << 10
	maxProxyURLBytes                  = 512
	defaultProxyTransportPoolCapacity = 256
	validatedHostAddrsTTL             = 5 * time.Second
	CustomRelayHeadersHeader          = "X-Canvas-Upstream-Headers"
	DefaultOutboundUserAgent          = "CanvasViewport/1.0 (+https://github.com/ddcat-ai/open-ai-canvas)"
)

type OutboundHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type outboundPolicyMode uint8

const (
	outboundPolicyStandard outboundPolicyMode = iota + 1
	outboundPolicyCustomRelay
)

type validatedHostContextKey struct{}

type validatedHostContextEntry struct {
	host             string
	policyMode       outboundPolicyMode
	allowPrivateHost bool
	envFingerprint   string
	addrs            []net.IP
	expiresAt        time.Time
	prev             *validatedHostContextEntry
}

func defaultLookupIP(ctx context.Context, network string, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, network, host)
}

var (
	lookupIPFunc               = defaultLookupIP
	outboundTransport          = newOutboundTransport(resolveOutboundHost)
	customRelayTransport       = newOutboundTransport(resolveCustomRelayHost)
	outboundProxyTransports    = newProxyTransportPool(defaultProxyTransportPoolCapacity)
	blockedCustomRelayPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
)

func parseOutboundURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return nil, BadAuthRequest("外部服务地址无效")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, BadAuthRequest("外部服务地址只支持 http/https")
	}
	if parsed.User != nil {
		return nil, BadAuthRequest("外部服务地址不允许包含认证信息")
	}
	return parsed, nil
}

func ValidateOutboundURL(rawURL string) (*url.URL, error) {
	parsed, err := parseOutboundURL(rawURL)
	if err != nil {
		return nil, err
	}
	if err := ValidateOutboundHost(parsed.Hostname()); err != nil {
		return nil, err
	}
	return parsed, nil
}

// WithValidatedOutboundURL 校验目标 URL 并将本次解析通过的 IP 列表以短 TTL 写入 ctx，
// 使紧随其后的直连拨号复用已校验结果而无需重复执行 DNS 查询。
func WithValidatedOutboundURL(ctx context.Context, rawURL string) (context.Context, *url.URL, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	parsed, err := parseOutboundURL(rawURL)
	if err != nil {
		return ctx, nil, err
	}
	host := normalizeOutboundHost(parsed.Hostname())
	allowPrivateHost := allowPrivateUpstreams() || AllowedPrivateUpstreamHost(host)
	if _, ok, err := validatedHostAddrsFromContext(ctx, host, outboundPolicyStandard, allowPrivateHost); ok {
		if err != nil {
			return ctx, nil, err
		}
		return ctx, parsed, nil
	}
	addresses, err := resolveOutboundHostWithMode(ctx, host, outboundPolicyStandard, allowPrivateHost)
	if err != nil {
		return ctx, nil, err
	}
	return withValidatedHostEntry(ctx, host, outboundPolicyStandard, allowPrivateHost, addresses), parsed, nil
}

// ValidateOutboundRequest 校验请求 URL 并返回携带短效已校验主机 IP 上下文的请求副本，
// 消除单次直连出站请求在校验阶段与 DialContext 阶段的重复 DNS 解析。
func ValidateOutboundRequest(req *http.Request) (*http.Request, error) {
	if req == nil || req.URL == nil {
		return nil, BadAuthRequest("外部服务地址无效")
	}
	nextCtx, _, err := WithValidatedOutboundURL(req.Context(), req.URL.String())
	if err != nil {
		return nil, err
	}
	return req.WithContext(nextCtx), nil
}

// WithValidatedHostAddrs 将指定主机已校验的候选 IP 绑定到标准出站策略上下文中（防御性拷贝且带短 TTL）；
// DialContext 命中时仍会按当前策略模式、主机名与 SSRF 规则二次核验。
func WithValidatedHostAddrs(ctx context.Context, host string, addrs []net.IP) context.Context {
	normalized := normalizeOutboundHost(host)
	allowPrivateHost := allowPrivateUpstreams() || AllowedPrivateUpstreamHost(normalized)
	return withValidatedHostEntry(ctx, normalized, outboundPolicyStandard, allowPrivateHost, addrs)
}

// 用户自定义渠道必须使用更严格的出口策略：不接受 URL 凭据，且仅部署者精确配置的
// 主机可以路由到私网或使用会明文传输 API Key 的 HTTP。
func ValidateCustomRelayURL(rawURL string) (*url.URL, error) {
	if len(strings.TrimSpace(rawURL)) > 4096 {
		return nil, BadAuthRequest("自定义渠道地址过长")
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" || !parsed.IsAbs() {
		return nil, BadAuthRequest("自定义渠道地址无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return nil, BadAuthRequest("自定义渠道地址只支持 http/https")
	}
	if scheme == "http" && !AllowedPrivateUpstreamHost(parsed.Hostname()) {
		return nil, BadAuthRequest("自定义渠道 HTTP 仅允许访问已配置的可信上游主机")
	}
	if parsed.User != nil {
		return nil, BadAuthRequest("自定义渠道地址不允许包含认证信息")
	}
	if parsed.Fragment != "" {
		return nil, BadAuthRequest("自定义渠道地址不允许包含片段")
	}
	if err := validateCustomRelayHost(parsed.Hostname()); err != nil {
		return nil, err
	}
	return parsed, nil
}

// ValidateProxyURL 校验渠道级代理地址。代理可以使用认证信息和私网地址，
// 因为它是管理员配置的出口，而不是用户可控的上游 URL。
func ValidateProxyURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, nil
	}
	if len(rawURL) > maxProxyURLBytes {
		return nil, BadAuthRequest("渠道代理地址过长")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return nil, BadAuthRequest("渠道代理地址无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "socks5" && scheme != "socks5h" && scheme != "http" && scheme != "https" {
		return nil, BadAuthRequest("渠道代理只支持 socks5/socks5h/http/https")
	}
	if parsed.Fragment != "" {
		return nil, BadAuthRequest("渠道代理地址不允许包含片段")
	}
	return parsed, nil
}

func validateOutboundRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxOutboundRedirects {
		return errors.New("外部服务重定向次数过多")
	}
	validated, err := ValidateOutboundRequest(req)
	if err != nil {
		return err
	}
	*req = *validated
	return nil
}

func OutboundHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     outboundTransport,
		Timeout:       timeout,
		CheckRedirect: validateOutboundRedirect,
	}
}

// OutboundHTTPClientWithProxy 创建使用指定代理的出站客户端。
// proxyURL 支持 socks5://、socks5h://、http://、https:// 格式；空字符串时回退到全局客户端。
// 相同规范化代理配置共享有界 *http.Transport 连接池，保持 TLS 会话与 HTTP/2 长连接复用。
func OutboundHTTPClientWithProxy(timeout time.Duration, proxyURL string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return OutboundHTTPClient(timeout)
	}
	transport := outboundProxyTransports.getOrCreate(proxyURL)
	if transport == nil {
		return &http.Client{Transport: rejectingTransport{err: errors.New("渠道代理地址无效")}, Timeout: timeout}
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: validateOutboundRedirect,
	}
}

type rejectingTransport struct{ err error }

func (t rejectingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func CustomRelayHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: customRelayTransport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("自定义渠道中转不允许重定向")
		},
	}
}

// ApplyDefaultOutboundHeaders 为所有服务端出网请求提供可识别的产品标识，
// 避免 Go 默认 User-Agent 被部分 WAF 或中转网关拦截。
func ApplyDefaultOutboundHeaders(req *http.Request) {
	if req != nil && strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", DefaultOutboundUserAgent)
	}
}

// NormalizeOutboundHeaders 是自定义 Header 的安全边界：允许业务/WAF 校验头和 User-Agent，
// 但禁止覆盖鉴权、请求体协议、代理内部头及 hop-by-hop 头。
func NormalizeOutboundHeaders(headers []OutboundHeader) ([]OutboundHeader, error) {
	if len(headers) > maxOutboundHeaderCount {
		return nil, BadAuthRequest(fmt.Sprintf("自定义请求头最多支持 %d 项", maxOutboundHeaderCount))
	}
	seen := make(map[string]struct{}, len(headers))
	normalized := make([]OutboundHeader, 0, len(headers))
	totalBytes := 0
	for _, item := range headers {
		name := strings.TrimSpace(item.Name)
		value := strings.TrimSpace(item.Value)
		if name == "" && value == "" {
			continue
		}
		if name == "" || !validOutboundHeaderName(name) {
			return nil, BadAuthRequest("自定义请求头名称无效，请使用标准 HTTP Header 名称")
		}
		name = http.CanonicalHeaderKey(name)
		lowerName := strings.ToLower(name)
		if blockedOutboundHeader(lowerName) {
			return nil, BadAuthRequest("请求头 " + name + " 由系统管理，不允许自定义")
		}
		if _, exists := seen[lowerName]; exists {
			return nil, BadAuthRequest("自定义请求头不能重复：" + name)
		}
		if !validOutboundHeaderValue(value) {
			return nil, BadAuthRequest("请求头 " + name + " 的值包含非法控制字符")
		}
		if len(name) > 128 || len(value) > 4096 {
			return nil, BadAuthRequest("单个自定义请求头名称或值过长")
		}
		totalBytes += len(name) + len(value)
		if totalBytes > maxOutboundHeaderBytes {
			return nil, BadAuthRequest("自定义请求头总大小不能超过 16KB")
		}
		seen[lowerName] = struct{}{}
		normalized = append(normalized, OutboundHeader{Name: name, Value: value})
	}
	return normalized, nil
}

func ApplyOutboundHeaders(req *http.Request, headers []OutboundHeader) {
	if req == nil {
		return
	}
	for _, item := range headers {
		req.Header.Set(item.Name, item.Value)
	}
}

func EncodeOutboundHeadersJSON(headers []OutboundHeader) (string, error) {
	normalized, err := NormalizeOutboundHeaders(headers)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(normalized)
	return string(data), err
}

func ParseOutboundHeadersJSON(raw string) ([]OutboundHeader, error) {
	if strings.TrimSpace(raw) == "" {
		return []OutboundHeader{}, nil
	}
	var headers []OutboundHeader
	if err := json.Unmarshal([]byte(raw), &headers); err != nil {
		return nil, errors.New("渠道自定义请求头配置损坏")
	}
	return NormalizeOutboundHeaders(headers)
}

func DecodeRelayOutboundHeaders(encoded string) ([]OutboundHeader, error) {
	if strings.TrimSpace(encoded) == "" {
		return []OutboundHeader{}, nil
	}
	if len(encoded) > (maxOutboundHeaderBytes*2)+1024 {
		return nil, BadAuthRequest("自定义请求头配置过大")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, BadAuthRequest("自定义请求头编码无效")
	}
	var headers []OutboundHeader
	if err := json.Unmarshal(data, &headers); err != nil {
		return nil, BadAuthRequest("自定义请求头格式无效")
	}
	return NormalizeOutboundHeaders(headers)
}

func validOutboundHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		char := name[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(char)) {
			continue
		}
		return false
	}
	return true
}

func validOutboundHeaderValue(value string) bool {
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char == '\t' || char >= 32 && char != 127 {
			continue
		}
		return false
	}
	return true
}

func blockedOutboundHeader(name string) bool {
	switch name {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "host", "content-length", "content-type", "accept", "connection", "proxy-connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade", "forwarded", "x-goog-api-key":
		return true
	}
	return strings.HasPrefix(name, "x-canvas-") || strings.HasPrefix(name, "x-forwarded-")
}

type proxyTransportPool struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	entries  map[string]*list.Element
	onEvict  func(*http.Transport)
}

type proxyTransportEntry struct {
	key       string
	transport *http.Transport
}

func newProxyTransportPool(capacity int) *proxyTransportPool {
	if capacity < 1 {
		capacity = defaultProxyTransportPoolCapacity
	}
	return &proxyTransportPool{
		capacity: capacity,
		ll:       list.New(),
		entries:  make(map[string]*list.Element, capacity),
	}
}

func (p *proxyTransportPool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ll.Len()
}

func (p *proxyTransportPool) getOrCreate(proxyURL string) *http.Transport {
	parsed, key, ok := normalizeProxyURLForPool(proxyURL)
	if !ok {
		return nil
	}

	p.mu.Lock()
	if elem, exists := p.entries[key]; exists {
		p.ll.MoveToFront(elem)
		transport := elem.Value.(*proxyTransportEntry).transport
		p.mu.Unlock()
		return transport
	}
	p.mu.Unlock()

	created := buildOutboundTransportWithProxy(parsed)
	if created == nil {
		return nil
	}

	var evicted []*http.Transport
	var onEvict func(*http.Transport)
	p.mu.Lock()
	if elem, exists := p.entries[key]; exists {
		p.ll.MoveToFront(elem)
		transport := elem.Value.(*proxyTransportEntry).transport
		p.mu.Unlock()
		return transport
	}
	for p.ll.Len() >= p.capacity {
		oldest := p.ll.Back()
		if oldest == nil {
			break
		}
		p.ll.Remove(oldest)
		entry := oldest.Value.(*proxyTransportEntry)
		delete(p.entries, entry.key)
		if entry.transport != nil {
			evicted = append(evicted, entry.transport)
		}
	}
	elem := p.ll.PushFront(&proxyTransportEntry{key: key, transport: created})
	p.entries[key] = elem
	onEvict = p.onEvict
	p.mu.Unlock()

	for _, tr := range evicted {
		tr.CloseIdleConnections()
		if onEvict != nil {
			onEvict(tr)
		}
	}
	return created
}

func normalizeProxyURLForPool(rawURL string) (*url.URL, string, bool) {
	parsed, err := ValidateProxyURL(rawURL)
	if err != nil || parsed == nil {
		return nil, "", false
	}
	normalized := *parsed
	normalized.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	normalized.Host = strings.ToLower(strings.TrimSpace(parsed.Host))
	return &normalized, normalized.String(), true
}

func newOutboundTransport(resolveHost func(context.Context, string) ([]net.IP, error)) *http.Transport {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: outboundProxyFromEnvironment,
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			// 代理地址由部署者通过环境变量配置；目标 URL 仍在请求前经过 SSRF 校验。
			if configuredProxyHost(host) {
				return dialer.DialContext(ctx, network, address)
			}
			addresses, err := resolveHost(ctx, host)
			if err != nil {
				return nil, err
			}
			targets := make([]string, 0, len(addresses))
			for _, ip := range addresses {
				addr, ok := netip.AddrFromSlice(ip)
				if !ok {
					continue
				}
				ipStr := addr.Unmap().String()
				if ipStr == "" {
					continue
				}
				targets = append(targets, net.JoinHostPort(ipStr, port))
			}
			return dialSequentialTargets(ctx, dialer, network, targets)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func dialSequentialTargets(ctx context.Context, dialer *net.Dialer, network string, targets []string) (net.Conn, error) {
	if len(targets) == 0 {
		return nil, errors.New("外部服务没有可用目标地址")
	}
	if network == "" {
		network = "tcp"
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	}
	var firstErr error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dialer.DialContext(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(err, ctxErr) {
				return nil, err
			}
			return nil, ctxErr
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

func outboundProxyFromEnvironment(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	return httpproxy.FromEnvironment().ProxyFunc()(req.URL)
}

// buildOutboundTransportWithProxy 创建使用指定代理的 Transport。
// 支持 socks5://、socks5h://、http://、https://；返回 nil 表示代理 URL 无效。
// 目标主机名不做本地解析，直接交给代理服务器远端解析（socks5 与 socks5h 同义）。
// 本地 DNS 在 TUN/VPN 模式下会返回 fake-ip，若先本地解析再经代
// 理连接会导致远端拿到不可达的假 IP；交给远端解析可避免此问题。
func buildOutboundTransportWithProxy(parsed *url.URL) *http.Transport {
	if parsed == nil {
		return nil
	}
	scheme := strings.ToLower(parsed.Scheme)
	forwardDialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

	transport := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}

	switch scheme {
	case "socks5", "socks5h":
		transport.Proxy = nil
		d, derr := proxy.FromURL(parsed, forwardDialer)
		if derr != nil {
			return nil
		}
		contextDialer, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil
		}
		transport.DialContext = contextDialer.DialContext
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
		transport.DialContext = forwardDialer.DialContext
	default:
		return nil
	}

	return transport
}

func configuredProxyHost(host string) bool {
	host = normalizeOutboundHost(host)
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		parsed, err := url.Parse(raw)
		if err == nil && normalizeOutboundHost(parsed.Hostname()) == host {
			return true
		}
	}
	return false
}

func ValidateOutboundHost(host string) error {
	_, err := resolveOutboundHost(context.Background(), host)
	return err
}

func validateCustomRelayHost(host string) error {
	_, err := resolveCustomRelayHost(context.Background(), host)
	return err
}

func resolveOutboundHost(ctx context.Context, host string) ([]net.IP, error) {
	host = normalizeOutboundHost(host)
	allowPrivateHost := allowPrivateUpstreams() || AllowedPrivateUpstreamHost(host)
	if addrs, ok, err := validatedHostAddrsFromContext(ctx, host, outboundPolicyStandard, allowPrivateHost); ok {
		return addrs, err
	}
	return resolveOutboundHostWithMode(ctx, host, outboundPolicyStandard, allowPrivateHost)
}

func resolveCustomRelayHost(ctx context.Context, host string) ([]net.IP, error) {
	host = normalizeOutboundHost(host)
	allowPrivateHost := AllowedPrivateUpstreamHost(host)
	if addrs, ok, err := validatedHostAddrsFromContext(ctx, host, outboundPolicyCustomRelay, allowPrivateHost); ok {
		return addrs, err
	}
	return resolveOutboundHostWithMode(ctx, host, outboundPolicyCustomRelay, allowPrivateHost)
}

func resolveOutboundHostWithPolicy(ctx context.Context, host string, allowPrivateHost bool) ([]net.IP, error) {
	return resolveOutboundHostWithMode(ctx, host, outboundPolicyStandard, allowPrivateHost)
}

func resolveOutboundHostWithMode(ctx context.Context, host string, mode outboundPolicyMode, allowPrivateHost bool) ([]net.IP, error) {
	if host == "" {
		return nil, BadAuthRequest("外部服务域名无效")
	}
	if !allowPrivateHost && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return nil, BadAuthRequest("不允许访问本机或内网地址")
	}
	addresses, err := lookupIPFunc(ctx, "ip", host)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		failure := kernel.WrapAppError(http.StatusBadGateway, "外部服务域名解析失败，请检查渠道域名和后端 DNS 配置", err)
		failure.Reason = kernel.ReasonUpstreamDNSFailed
		return nil, failure
	}
	if err := verifyResolvedHostAddresses(host, addresses, mode, allowPrivateHost); err != nil {
		return nil, err
	}
	return cloneIPSlice(addresses), nil
}

func verifyResolvedHostAddresses(host string, addresses []net.IP, mode outboundPolicyMode, allowPrivateHost bool) error {
	if host == "" {
		return BadAuthRequest("外部服务域名无效")
	}
	if !allowPrivateHost && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return BadAuthRequest("不允许访问本机或内网地址")
	}
	if len(addresses) == 0 {
		failure := kernel.NewAppError(http.StatusBadGateway, "外部服务域名没有可用地址，请检查渠道域名和后端 DNS 配置")
		failure.Reason = kernel.ReasonUpstreamDNSFailed
		return failure
	}
	for _, ip := range addresses {
		if _, ok := netip.AddrFromSlice(ip); !ok {
			return BadAuthRequest("外部服务地址无效")
		}
	}
	if !allowPrivateHost {
		for _, ip := range addresses {
			if blockedOutboundIP(ip) {
				return BadAuthRequest("不允许访问本机、内网或链路本地地址")
			}
		}
		if mode == outboundPolicyCustomRelay {
			for _, ip := range addresses {
				if blockedCustomRelayIP(ip) {
					return BadAuthRequest("不允许访问保留地址或特殊用途地址")
				}
			}
		}
	}
	return nil
}

func withValidatedHostEntry(ctx context.Context, host string, mode outboundPolicyMode, allowPrivateHost bool, addrs []net.IP) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	host = normalizeOutboundHost(host)
	cloned := cloneIPSlice(addrs)
	if host == "" || len(cloned) == 0 {
		return ctx
	}
	prev, _ := ctx.Value(validatedHostContextKey{}).(*validatedHostContextEntry)
	entry := &validatedHostContextEntry{
		host:             host,
		policyMode:       mode,
		allowPrivateHost: allowPrivateHost,
		envFingerprint:   outboundEnvPolicyFingerprint(),
		addrs:            cloned,
		expiresAt:        time.Now().Add(validatedHostAddrsTTL),
		prev:             prev,
	}
	return context.WithValue(ctx, validatedHostContextKey{}, entry)
}

func validatedHostAddrsFromContext(ctx context.Context, host string, mode outboundPolicyMode, allowPrivateHost bool) ([]net.IP, bool, error) {
	if ctx == nil {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	entry, _ := ctx.Value(validatedHostContextKey{}).(*validatedHostContextEntry)
	if entry == nil {
		return nil, false, nil
	}
	now := time.Now()
	fingerprint := outboundEnvPolicyFingerprint()
	for cur := entry; cur != nil; cur = cur.prev {
		if cur.host != host || cur.policyMode != mode || cur.allowPrivateHost != allowPrivateHost || cur.envFingerprint != fingerprint {
			continue
		}
		if !now.Before(cur.expiresAt) || len(cur.addrs) == 0 {
			continue
		}
		if err := verifyResolvedHostAddresses(host, cur.addrs, mode, allowPrivateHost); err != nil {
			return nil, true, err
		}
		return cloneIPSlice(cur.addrs), true, nil
	}
	return nil, false, nil
}

func outboundEnvPolicyFingerprint() string {
	allowAll := "0"
	if allowPrivateUpstreams() {
		allowAll = "1"
	}
	return allowAll + "|" + strings.ToLower(strings.TrimSpace(os.Getenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS")))
}

func cloneIPSlice(addrs []net.IP) []net.IP {
	if len(addrs) == 0 {
		return nil
	}
	copied := make([]net.IP, 0, len(addrs))
	for _, ip := range addrs {
		if _, ok := netip.AddrFromSlice(ip); !ok {
			continue
		}
		dup := make(net.IP, len(ip))
		copy(dup, ip)
		copied = append(copied, dup)
	}
	if len(copied) == 0 {
		return nil
	}
	return copied
}

func blockedOutboundIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalMulticast() || address.IsLinkLocalUnicast() || address.IsUnspecified() || address.IsMulticast()
}

func blockedCustomRelayIP(ip net.IP) bool {
	if blockedOutboundIP(ip) {
		return true
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	for _, prefix := range blockedCustomRelayPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func allowPrivateUpstreams() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS")))
	return value == "1" || value == "true" || value == "yes"
}

// allowedPrivateUpstreamHost lets operators pin only explicitly trusted upstream
// hostnames to an internal route without disabling SSRF protection for every URL.
func AllowedPrivateUpstreamHost(host string) bool {
	host = normalizeOutboundHost(host)
	if host == "" {
		return false
	}
	for _, configured := range strings.Split(os.Getenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS"), ",") {
		if normalizeOutboundHost(configured) == host {
			return true
		}
	}
	return false
}

func normalizeOutboundHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}
