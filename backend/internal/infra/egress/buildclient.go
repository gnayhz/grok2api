package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "github.com/bdandy/go-socks4"
	"github.com/chenyme/grok2api/backend/internal/infra/buildtransport"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
	xproxy "golang.org/x/net/proxy"
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

// newSessionBuildClient 为单个会话构造「单连接钉扎」的 Build 客户端:
// MaxConnsPerHost=1 强制整个会话复用同一条上游连接(HTTP/2 多路复用承接
// 并发流)。上游提示缓存按「连接→后端实例」亲和复用,共享连接池里任何
// 并行调用多开出的第二条连接都会让后续请求在两条连接间轮换,各自的前缀
// 缓存互相缺失,表现为会话中途连续冷启动。会话独占一条连接后,轮换维度
// 被彻底消除,连接只会在自身死亡(上游关闭/健康探测失败)时更换。
func newSessionBuildClient(proxyURL string, responseHeaderTimeout time.Duration, onDial func()) (*http.Client, error) {
	return newBuildClientConfigured(proxyURL, responseHeaderTimeout, false, true, onDial)
}

func newBuildClientWithOptions(proxyURL string, responseHeaderTimeout time.Duration, environmentProxy bool) (*http.Client, error) {
	return newBuildClientConfigured(proxyURL, responseHeaderTimeout, environmentProxy, false, nil)
}

func newBuildClientConfigured(proxyURL string, responseHeaderTimeout time.Duration, environmentProxy, sessionPinned bool, onDial func()) (*http.Client, error) {
	maxConnsPerHost, maxIdleConnsPerHost := 256, 128
	if sessionPinned {
		maxConnsPerHost, maxIdleConnsPerHost = 1, 1
	}
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           direct.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxConnsPerHost,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       buildtransport.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	if environmentProxy {
		transport.Proxy = http.ProxyFromEnvironment
	}
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析 Grok Build 出口代理: %w", err)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(parsed)
		case "socks4", "socks4a", "socks5", "socks5h":
			dialer, err := xproxy.FromURL(parsed, direct)
			if err != nil {
				return nil, fmt.Errorf("创建 Grok Build SOCKS 代理: %w", err)
			}
			transport.DialContext = dialContext(dialer)
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
	if sessionPinned && onDial != nil {
		inner := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			onDial()
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

func dialContext(dialer xproxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if contextual, ok := dialer.(xproxy.ContextDialer); ok {
		return contextual.DialContext
	}
	type result struct {
		connection net.Conn
		err        error
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		completed := make(chan result, 1)
		go func() {
			connection, err := dialer.Dial(network, address)
			completed <- result{connection: connection, err: err}
		}()
		select {
		case value := <-completed:
			return value.connection, value.err
		case <-ctx.Done():
			go func() {
				value := <-completed
				if value.connection != nil {
					_ = value.connection.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}
}
