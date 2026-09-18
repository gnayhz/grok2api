package buildtransport

import (
	"errors"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

const (
	// IdleConnTimeout 保留健康空闲连接，减少轮间重复建连和握手。
	// 连接复用、出口绑定和上游提示缓存是独立事实：同一连接仍可能未命中，
	// 新连接也可能命中，不能从连接状态推断上游后端或缓存归属。
	// 150s 覆盖常见轮间间隔；半死 HTTP/2 连接由下方 PING 检测。
	IdleConnTimeout = 150 * time.Second
	// HTTP2ReadIdleTimeout periodically probes an otherwise idle HTTP/2
	// connection. Go's default is zero, which leaves half-dead pooled
	// connections undetected until a request lands on them.
	HTTP2ReadIdleTimeout = 20 * time.Second
	HTTP2PingTimeout     = 10 * time.Second
)

// ConfigureHTTP2Health enables active PING health checks on a Build transport.
// It must be called after proxy and dialer options have been applied.
func ConfigureHTTP2Health(transport *http.Transport) (*http2.Transport, error) {
	if transport == nil {
		return nil, errors.New("Build HTTP transport is nil")
	}
	h2, err := http2.ConfigureTransports(transport)
	if err != nil {
		return nil, err
	}
	h2.ReadIdleTimeout = HTTP2ReadIdleTimeout
	h2.PingTimeout = HTTP2PingTimeout
	return h2, nil
}
