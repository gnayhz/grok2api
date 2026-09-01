package buildtransport

import (
	"errors"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

const (
	// IdleConnTimeout 保留空闲连接的时长。上游提示缓存按「连接→后端实例」
	// 亲和复用(实测换连接即冷、同连接跨账号也热),主动退役一条仍然健康的
	// 连接等于放弃整段会话缓存:轮间隙超过空闲窗口时,下一轮必然落到新
	// 后端、全量重算(线上表现为中途突然只剩公共头部命中)。半死连接的
	// 检测交给下方的 HTTP/2 PING 健康检查(20s 探测+10s 超时),因此这里
	// 只需低于上游代理自身的空闲关闭窗口即可,取 150s 覆盖常见的思考/
	// 阅读间隙。历史上取 30s 是在 PING 健康检查存在之前规避半死连接的
	// 保守值,代价是 >30s 的轮间隙稳定丢缓存。
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
