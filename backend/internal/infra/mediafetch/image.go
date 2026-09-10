package mediafetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/netguard"
)

const (
	ingestFetchTimeout   = 20 * time.Second
	ingestResolveTimeout = 3 * time.Second
	ingestMaxRedirects   = 5
)

// ImageSource keeps the direct public-only import transport independent of
// Provider accounts, proxy leases and the incoming HTTP server.
type ImageSource struct {
	resolver  importURLResolver
	newClient func(*importTarget) (*http.Client, *http.Transport)
}

func NewImageSource() *ImageSource {
	return &ImageSource{resolver: net.DefaultResolver, newClient: newIngestHTTPClient}
}

var _ mediadomain.InputImageSource = (*ImageSource)(nil)

type importURLResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// importTarget 把用户 URL 的请求语义与实际连接目标分离：fetchURL 只包含已验证的公网 IP，
// hostHeader/serverName 则保留原始虚拟主机与 TLS 证书校验语义。
type importTarget struct {
	fetchURL   *url.URL
	hostHeader string
	serverName string
}

// ssrfSafeControl 在 TCP 连接建立前检查目标 IP，拒绝私有/环回/链路本地/未指定/多播地址及云元数据地址。
// 返回的错误包裹 mediadomain.ErrInputImageURLBlocked，便于上层用 errors.Is 识别为"地址被拒绝"。
func ssrfSafeControl(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" && network != "tcp" {
		return fmt.Errorf("不支持的网络类型 %q: %w", network, mediadomain.ErrInputImageURLBlocked)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("解析目标地址失败 %q: %w", address, mediadomain.ErrInputImageURLBlocked)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("目标地址不是有效 IP %q: %w", host, mediadomain.ErrInputImageURLBlocked)
	}
	if !isPublicIP(ip.Unmap()) {
		return fmt.Errorf("拒绝访问非公网地址 %s: %w", host, mediadomain.ErrInputImageURLBlocked)
	}
	return nil
}

// isPublicIP 仅当 IP 是可路由公网地址时返回 true。
func isPublicIP(ip netip.Addr) bool {
	return netguard.IsPublicAddress(ip)
}

// resolveImportTarget 先解析并校验全部 DNS 结果，再固定本次请求的连接 IP。
// 这同时消除了校验与拨号之间的 DNS rebinding 窗口；ssrfSafeControl 仍在拨号时做第二道校验。
func resolveImportTarget(ctx context.Context, parsed *url.URL, resolver importURLResolver) (*importTarget, error) {
	if !mediadomain.AllowedInputImageURL(parsed) {
		return nil, mediadomain.ErrInputImageURLBlocked
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || strings.Contains(host, "%") {
		return nil, mediadomain.ErrInputImageURLBlocked
	}

	var addresses []netip.Addr
	if address, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{address.Unmap()}
	} else {
		resolveCtx, cancel := context.WithTimeout(ctx, ingestResolveTimeout)
		defer cancel()
		resolved, err := resolver.LookupNetIP(resolveCtx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("解析图片主机失败: %w", err)
		}
		if len(resolved) == 0 {
			return nil, errors.New("解析图片主机失败: DNS 未返回地址")
		}
		addresses = make([]netip.Addr, 0, len(resolved))
		for _, address := range resolved {
			addresses = append(addresses, address.Unmap())
		}
	}
	for _, address := range addresses {
		if !isPublicIP(address) {
			return nil, fmt.Errorf("图片主机解析到非公网地址 %s: %w", address, mediadomain.ErrInputImageURLBlocked)
		}
	}

	port := parsed.Port()
	if port == "" {
		if strings.EqualFold(parsed.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	fetchURL := *parsed
	fetchURL.Scheme = strings.ToLower(parsed.Scheme)
	fetchURL.Host = net.JoinHostPort(addresses[0].String(), port)
	fetchURL.Fragment = ""
	return &importTarget{fetchURL: &fetchURL, hostHeader: parsed.Host, serverName: host}, nil
}

func newIngestHTTPClient(target *importTarget) (*http.Client, *http.Transport) {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
			Control:   ssrfSafeControl,
		}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, ServerName: target.serverName},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		ExpectContinueTimeout:  1 * time.Second,
		MaxResponseHeaderBytes: 1 << 20,
		MaxIdleConns:           1,
		MaxConnsPerHost:        1,
		IdleConnTimeout:        30 * time.Second,
		ForceAttemptHTTP2:      true,
	}
	client := &http.Client{
		Transport: transport,
		// 重定向必须回到 FetchImage 重新解析和固定目标，禁止 net/http 自动跟随。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, transport
}

func isImportRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

// FetchImage owns all DNS, body and connection resources until bounded bytes are returned.
// The application supplies the byte limit; each redirect must remain public.
func (s *ImageSource) FetchImage(ctx context.Context, initial *url.URL, maxBytes int64) ([]byte, error) {
	if !mediadomain.AllowedInputImageURL(initial) || maxBytes <= 0 {
		return nil, mediadomain.ErrInputImageURLBlocked
	}
	parsed := initial
	fetchCtx, cancel := context.WithTimeout(ctx, ingestFetchTimeout)
	defer cancel()

	for redirects := 0; ; redirects++ {
		target, err := resolveImportTarget(fetchCtx, parsed, s.resolver)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, target.fetchURL.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Host = target.hostHeader
		req.Header.Set("Accept", "image/*")
		req.Header.Set("User-Agent", "grok2api-media-importer/1.0")

		client, transport := s.newClient(target)
		resp, err := client.Do(req)
		if err != nil {
			transport.CloseIdleConnections()
			if errors.Is(err, mediadomain.ErrInputImageURLBlocked) {
				return nil, mediadomain.ErrInputImageURLBlocked
			}
			return nil, err
		}

		if isImportRedirect(resp.StatusCode) && resp.Header.Get("Location") != "" {
			_ = resp.Body.Close()
			transport.CloseIdleConnections()
			if redirects >= ingestMaxRedirects {
				return nil, errors.New("重定向次数过多")
			}
			next, err := parsed.Parse(resp.Header.Get("Location"))
			if err != nil || !mediadomain.AllowedInputImageURL(next) {
				return nil, fmt.Errorf("重定向地址无效: %w", mediadomain.ErrInputImageURLBlocked)
			}
			parsed = next
			continue
		}

		data, err := readImportedImage(resp, maxBytes)
		transport.CloseIdleConnections()
		return data, err
	}
}

func readImportedImage(resp *http.Response, maxBytes int64) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, mediadomain.ErrInputImageTooLarge
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, mediadomain.ErrInputImageTooLarge
	}
	return data, nil
}
