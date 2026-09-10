package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/buildtransport"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/pkg/proxydial"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

// newBuildClient keeps Grok Build on the standard Go HTTP/TLS stack used by
// the official CLI-facing transport. Browser TLS impersonation is reserved for
// Grok Web, where the browser fingerprint and User-Agent belong together.
func newBuildClient(proxyURL string, responseHeaderTimeout time.Duration) (*http.Client, error) {
	return newBuildClientWithOptions(proxyURL, responseHeaderTimeout, false)
}

// newBuildEnvironmentClient preserves the process-wide Build direct
// transport's HTTP_PROXY/HTTPS_PROXY behavior while giving the caller an
// independent connection pool.
func newBuildEnvironmentClient(responseHeaderTimeout time.Duration) (*http.Client, error) {
	return newBuildClientWithOptions("", responseHeaderTimeout, true)
}

// newSessionBuildClient limits an allowed session pool to one connection per
// host, with HTTP/2 multiplexing when supported. The registry owns account
// partitioning and policy retirement; fresh policy cannot use this pool.
// Reuse helps upstream affinity but cannot guarantee a cache hit.
func newSessionBuildClient(proxyURL string, responseHeaderTimeout time.Duration, onDial func(), budget ...*netbudget.Runtime) (*http.Client, error) {
	return newBuildClientConfigured(proxyURL, responseHeaderTimeout, buildConnectionOptions{sessionPinned: true, onDial: onDial}, budget...)
}

func newBuildClientWithOptions(proxyURL string, responseHeaderTimeout time.Duration, environmentProxy bool) (*http.Client, error) {
	return newBuildClientConfigured(proxyURL, responseHeaderTimeout, buildConnectionOptions{environmentProxy: environmentProxy})
}

type buildConnectionOptions struct {
	environmentProxy bool
	sessionPinned    bool
	freshConnection  bool
	onDial           func()
}

func newBuildClientConfigured(proxyURL string, responseHeaderTimeout time.Duration, options buildConnectionOptions, budget ...*netbudget.Runtime) (*http.Client, error) {
	maxConnsPerHost, maxIdleConnsPerHost := 256, 128
	if options.sessionPinned && !options.freshConnection {
		maxConnsPerHost, maxIdleConnsPerHost = 1, 1
	}
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:             nil,
		DialContext:       direct.DialContext,
		ForceAttemptHTTP2: true,
		// A separate transport identity with keepalive disabled prevents both
		// idle reuse and HTTP/2 multiplexing of concurrent fresh calls.
		DisableKeepAlives:     options.freshConnection,
		MaxIdleConns:          maxConnsPerHost,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       buildtransport.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	if options.environmentProxy {
		transport.Proxy = http.ProxyFromEnvironment
	}
	if strings.TrimSpace(proxyURL) != "" {
		// An explicit exit replaces the environment policy for every protocol,
		// including dial-based SOCKS/tunnels which do not use Transport.Proxy.
		transport.Proxy = nil
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析 Grok Build 出口代理: %w", err)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(parsed)
		case "socks4", "socks4a", "socks5", "socks5h":
			dialer, err := proxydial.New(proxyURL)
			if err != nil {
				return nil, fmt.Errorf("创建 Grok Build SOCKS 代理: %w", err)
			}
			transport.DialContext = dialer.DialContext
		case "trojan", "vless", "ss", "vmess":
			dialer, err := tunnelproxy.NewDialer(proxyURL)
			if err != nil {
				return nil, fmt.Errorf("创建 Grok Build 隧道代理: %w", err)
			}
			transport.DialContext = dialer.DialContext
		default:
			return nil, fmt.Errorf("Grok Build 不支持代理协议 %q", parsed.Scheme)
		}
	}
	// 会话客户端的拨号观测:每次向上游代理新建 TCP 连接时回调一次,
	// 用于从日志侧核对「同一会话是否真的全程复用一条连接」。必须在
	// ConfigureHTTP2Health 之前安装——HTTP/2 层在配置时会固化拨号路径,
	// 事后替换 http1 字段对 h2 连接不生效。
	if len(budget) > 0 && budget[0] != nil {
		inner := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return budget[0].Dial(ctx, inner, network, address)
		}
	}
	if options.sessionPinned && options.onDial != nil {
		inner := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			options.onDial()
			return inner(ctx, network, address)
		}
	}
	if _, err := buildtransport.ConfigureHTTP2Health(transport); err != nil {
		return nil, fmt.Errorf("配置 Grok Build HTTP/2 健康探测: %w", err)
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
