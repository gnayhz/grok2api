package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/netguard"
	"github.com/chenyme/grok2api/backend/internal/pkg/proxydial"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

const (
	maxSubscriptionBytes     = 2 << 20
	maxSubscriptionHops      = 3
	subscriptionFetchTimeout = 20 * time.Second
)

// SubscriptionFetcher downloads remote proxy subscription content. URL and
// node configuration policy (normalization, generation, rotation accounting)
// stays in the application; this component owns transport: dialing, proxy
// schemes, redirect handling, SSRF narrowing for proxied fetches and size
// limits.
type SubscriptionFetcher struct {
	owner     ControlTransportOwner
	normalize func(value string) (string, error)
}

func NewSubscriptionFetcher(owner ControlTransportOwner, normalize func(string) (string, error)) *SubscriptionFetcher {
	return &SubscriptionFetcher{owner: owner, normalize: normalize}
}

// FetchProxySubscription pulls the subscription body. Every target and
// redirect of a proxied fetch must resolve exclusively to public addresses.
func (f *SubscriptionFetcher) FetchProxySubscription(ctx context.Context, value string, viaProxy string) ([]byte, error) {
	return fetchProxySubscription(ctx, value, viaProxy, f.owner, f.normalize)
}

func fetchProxySubscription(ctx context.Context, value string, viaProxy string, owner ControlTransportOwner, normalize func(string) (string, error)) ([]byte, error) {
	normalized, err := normalize(value)
	if err != nil {
		return nil, err
	}
	transport, err := subscriptionTransport(viaProxy)
	if err != nil {
		return nil, err
	}
	defer transport.CloseIdleConnections()
	var requestTransport http.RoundTripper = transport
	if owner != nil {
		managed, closeTransport, err := owner.ManageHTTPTransport(ctx, transport)
		if err != nil {
			return nil, err
		}
		defer closeTransport()
		requestTransport = managed
	}
	proxied := strings.TrimSpace(viaProxy) != ""
	client := &http.Client{
		Transport: requestTransport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= maxSubscriptionHops {
				return errors.New("订阅重定向次数过多")
			}
			redirectURL, err := normalize(request.URL.String())
			if err != nil {
				return errors.New("订阅重定向地址无效")
			}
			if proxied {
				if err := validatePublicSubscriptionTarget(request.Context(), redirectURL); err != nil {
					return errors.New("订阅重定向地址不能指向内网")
				}
			}
			return nil
		},
	}
	requestCtx, cancel := context.WithTimeout(ctx, subscriptionFetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, normalized, nil)
	if err != nil {
		return nil, err
	}
	if proxied {
		if err := validatePublicSubscriptionTarget(requestCtx, normalized); err != nil {
			return nil, err
		}
	}
	request.Header.Set("Accept", "text/plain, text/*;q=0.9, */*;q=0.1")
	request.Header.Set("User-Agent", "Clash.Meta")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("订阅服务返回 HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSubscriptionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxSubscriptionBytes {
		return nil, errors.New("订阅内容超过大小限制")
	}
	return body, nil
}

func subscriptionTransport(viaProxy string) (*http.Transport, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           publicDialContext(net.DefaultResolver),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       15 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	viaProxy = strings.TrimSpace(viaProxy)
	if viaProxy == "" {
		return transport, nil
	}
	parsed, err := url.Parse(viaProxy)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("订阅拉取代理地址无效")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
		// Dial the proxy endpoint itself; private admin proxies are allowed.
		transport.DialContext = direct.DialContext
	case "socks4", "socks4a", "socks5", "socks5h":
		dialer, err := proxydial.New(viaProxy)
		if err != nil {
			return nil, fmt.Errorf("创建订阅拉取 SOCKS 代理: %w", err)
		}
		transport.DialContext = dialer.DialContext
	case "trojan", "vless", "ss", "vmess":
		dialer, err := tunnelproxy.NewDialer(viaProxy)
		if err != nil {
			return nil, fmt.Errorf("创建订阅拉取隧道代理: %w", err)
		}
		transport.DialContext = dialer.DialContext
	default:
		return nil, errors.New("订阅拉取代理协议不受支持")
	}
	return transport, nil
}

func validatePublicSubscriptionTarget(ctx context.Context, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return errors.New("订阅地址格式无效")
	}
	host := strings.Trim(strings.TrimSpace(parsed.Hostname()), "[]")
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		if !netguard.IsPublicAddress(address) {
			return errors.New("订阅地址不能指向内网")
		}
		return nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("解析订阅地址: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("订阅地址没有可用的公网 IP")
	}
	for _, address := range addresses {
		if !netguard.IsPublicAddress(address) {
			return errors.New("订阅地址不能指向内网")
		}
	}
	return nil
}

func publicDialContext(resolver *net.Resolver) func(context.Context, string, string) (net.Conn, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolvePublicAddresses(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
		var lastErr error
		for _, ip := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("订阅地址没有可用的公网 IP")
	}
}

func resolvePublicAddresses(ctx context.Context, resolver *net.Resolver, host string) ([]netip.Addr, error) {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if parsed, err := netip.ParseAddr(host); err == nil {
		if !netguard.IsPublicAddress(parsed) {
			return nil, errors.New("订阅地址不能指向内网")
		}
		return []netip.Addr{parsed.Unmap()}, nil
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	public := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		if netguard.IsPublicAddress(address) {
			public = append(public, address.Unmap())
		}
	}
	if len(public) == 0 {
		return nil, errors.New("订阅地址不能指向内网")
	}
	return public, nil
}
