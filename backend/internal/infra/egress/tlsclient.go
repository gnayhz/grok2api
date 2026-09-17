package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptrace "github.com/bogdanfinn/fhttp/httptrace"
	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/browsertransport"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/proxydial"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

type browserClient struct {
	inner     *fhttp.Client
	transport *browsertransport.Transport
}

var chromeMajorPattern = regexp.MustCompile(`(?i)Chrome/(\d+)`)

func (l *Lease) DialWebSocket(ctx context.Context, endpoint string, headers fhttp.Header, handshakeTimeout time.Duration) (*WebSocket, *fhttp.Response, error) {
	return l.dialWebSocket(ctx, endpoint, headers, handshakeTimeout, true)
}

// DialWebSocketDeferredForbidden leaves a 403 handshake response for the
// caller to classify before invalidating the browser-session Clearance.
func (l *Lease) DialWebSocketDeferredForbidden(ctx context.Context, endpoint string, headers fhttp.Header, handshakeTimeout time.Duration) (*WebSocket, *fhttp.Response, error) {
	return l.dialWebSocket(ctx, endpoint, headers, handshakeTimeout, false)
}

func (l *Lease) dialWebSocket(ctx context.Context, endpoint string, headers fhttp.Header, handshakeTimeout time.Duration, invalidateForbidden bool) (*WebSocket, *fhttp.Response, error) {
	if l == nil || l.browser == nil {
		return nil, nil, errors.New("当前出口客户端不支持浏览器 WebSocket")
	}
	for attempt := 0; ; attempt++ {
		attemptCtx := ctx
		if attempt > 0 {
			attemptCtx = WithPhysicalCallStage(attemptCtx, "connection_retry")
		}
		attemptCtx = attemptmeta.Begin(attemptCtx, l.attemptPath())
		finish := func() {}
		if l.clientHandle != nil {
			callCtx, done, err := l.clientHandle.begin(attemptCtx)
			if err != nil {
				return nil, nil, err
			}
			attemptCtx, finish = callCtx, done
		}
		if err := beginPhysicalCall(attemptCtx); err != nil {
			finish()
			return nil, nil, err
		}
		dialer := &websocket.Dialer{
			HandshakeTimeout:  handshakeTimeout,
			NetDialTLSContext: completionDial(attemptCtx, l.browser.transport.DialWebSocketTLS, finish),
			NetDialContext:    completionDial(attemptCtx, l.browser.transport.DialContext, finish),
		}
		connection, response, err := dialer.DialContext(attemptCtx, endpoint, headers)
		recordWebSocketHandshake(attemptCtx, endpoint, response, err)
		if err == nil || !l.proxyPool || attempt >= proxyPoolRetryLimit || !safeProxyConnectionFailure(err, fhttpResponseAsHTTP(response)) {
			if l.proxyPool && safeProxyConnectionFailure(err, fhttpResponseAsHTTP(response)) {
				l.browser.CloseIdleConnections()
			}
			if invalidateForbidden && response != nil && response.StatusCode == http.StatusForbidden && l.clearanceManager != nil && l.clearanceKey != "" {
				l.InvalidateClearance()
			}
			if err != nil {
				finish()
			}
			if connection != nil && err == nil {
				return newPhysicalWebSocket(attemptCtx, connection), response, nil
			}
			return nil, response, neterrorpkg.MarkTransport(err, neterrorpkg.PhaseWebSocket)
		}
		finish()
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		l.browser.CloseIdleConnections()
	}
}

func fhttpResponseAsHTTP(response *fhttp.Response) *http.Response {
	if response == nil {
		return nil
	}
	return &http.Response{StatusCode: response.StatusCode, Header: http.Header(response.Header), Body: response.Body}
}

func newBrowserClientWithBudget(proxyURL, userAgent string, budget *netbudget.Runtime) (*browserClient, error) {
	var dial browsertransport.DialContextFunc
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析浏览器出口代理: %w", err)
		}
		var proxy interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}
		if tunnelproxy.IsSupportedScheme(parsed.Scheme) {
			proxy, err = tunnelproxy.NewDialer(proxyURL)
		} else {
			proxy, err = proxydial.New(proxyURL)
		}
		if err != nil {
			return nil, err
		}
		dial = proxy.DialContext
	}
	if budget != nil {
		inner := dial
		if inner == nil {
			inner = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return budget.Dial(ctx, netbudget.DialFunc(inner), network, address)
		}
	}
	transport := browsertransport.New(browsertransport.Config{Profile: browserProfile(userAgent), DialContext: dial})
	client := &fhttp.Client{Transport: transport, CheckRedirect: func(*fhttp.Request, []*fhttp.Request) error { return fhttp.ErrUseLastResponse }}
	return &browserClient{inner: client, transport: transport}, nil
}

func browserProfile(userAgent string) profiles.ClientProfile {
	match := chromeMajorPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(match) == 2 {
		if profile, ok := profiles.MappedTLSClients["chrome_"+match[1]]; ok {
			return profile
		}
		major, err := strconv.Atoi(match[1])
		if err == nil {
			bestMajor, bestDistance := 0, int(^uint(0)>>1)
			for _, candidate := range []int{146, 144, 133, 131, 124, 120, 117} {
				distance := candidate - major
				if distance < 0 {
					distance = -distance
				}
				if distance < bestDistance {
					bestMajor, bestDistance = candidate, distance
				}
			}
			if profile, ok := profiles.MappedTLSClients[fmt.Sprintf("chrome_%d", bestMajor)]; ok {
				return profile
			}
		}
	}
	return profiles.Chrome_146
}

// browserClientDefaultRequestTimeout bounds requests without a caller deadline.
// Establishment has its own budget in browsertransport and is removed before
// streaming. A caller deadline continues to control the complete request.
var browserClientDefaultRequestTimeout = 10 * time.Minute

// cancelOnCloseBody 把 ctx 取消绑定到响应体 Close:Do 返回后 body 仍在
// 读取,不能在 Do 出口取消——那会截断在途下载。
type cancelOnCloseBody struct {
	io.ReadCloser
	ctx      context.Context
	finished atomic.Bool
	cancel   context.CancelFunc
	once     sync.Once
}

// finish releases the locally installed timeout after a terminal read/Close.
// Its cancellation must not turn a later ordinary EOF into context.Canceled.
func (b *cancelOnCloseBody) finish() {
	b.once.Do(func() { b.finished.Store(true); b.cancel() })
}

func (b *cancelOnCloseBody) Read(p []byte) (int, error) {
	if !b.finished.Load() {
		if err := b.ctx.Err(); err != nil {
			b.finish()
			return 0, err
		}
	}
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		// fhttp can close a response before Read observes the canceled request.
		// Recover that request cause before our own cleanup cancels its timeout.
		if !b.finished.Load() {
			if ctxErr := b.ctx.Err(); ctxErr != nil {
				err = ctxErr
			}
		}
		b.finish()
	}
	return n, err
}

func (b *cancelOnCloseBody) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}

func (c *browserClient) Do(request *http.Request) (*http.Response, error) {
	cancel := func() {}
	if _, ok := request.Context().Deadline(); !ok {
		var bounded context.Context
		bounded, cancel = context.WithTimeout(request.Context(), browserClientDefaultRequestTimeout)
		request = request.Clone(bounded)
	}
	frequest, err := toFHTTPRequest(request)
	if err != nil {
		cancel()
		return nil, err
	}
	fresponse, err := c.inner.Do(frequest)
	if err != nil {
		cancel()
		return nil, err
	}
	response := fromFHTTPResponse(fresponse)
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, ctx: request.Context(), cancel: cancel}
	return response, nil
}

func fromFHTTPResponse(fresponse *fhttp.Response) *http.Response {
	header := http.Header(fresponse.Header).Clone()
	contentLength := fresponse.ContentLength
	if fresponse.Uncompressed {
		header.Del("Content-Encoding")
		header.Del("Content-Length")
		contentLength = -1
	}
	transferEncoding := append([]string(nil), fresponse.TransferEncoding...)
	return &http.Response{
		Status: fresponse.Status, StatusCode: fresponse.StatusCode, Proto: fresponse.Proto,
		ProtoMajor: fresponse.ProtoMajor, ProtoMinor: fresponse.ProtoMinor, Header: header,
		Body: fresponse.Body, ContentLength: contentLength, TransferEncoding: transferEncoding,
		// fhttp 在读取 Body 到 EOF 时原地填充 Trailer，因此这里必须保留共享 map。
		Close: fresponse.Close, Uncompressed: fresponse.Uncompressed, Trailer: http.Header(fresponse.Trailer),
	}
}

func (c *browserClient) CloseIdleConnections() {
	if c != nil && c.inner != nil {
		c.inner.CloseIdleConnections()
	}
}

func toFHTTPRequest(request *http.Request) (*fhttp.Request, error) {
	var body io.Reader
	if request.Body != nil {
		body = request.Body
	}
	ctx := request.Context()
	// fhttp has a distinct trace context key. Bridge submission callbacks so
	// browser retries cannot replay a POST that has already reached the origin.
	if trace := httptrace.ContextClientTrace(ctx); trace != nil {
		bridge := &fhttptrace.ClientTrace{}
		bridge.GetConn = trace.GetConn
		bridge.ConnectStart = trace.ConnectStart
		bridge.ConnectDone = trace.ConnectDone
		bridge.TLSHandshakeStart = trace.TLSHandshakeStart
		if trace.TLSHandshakeDone != nil {
			bridge.TLSHandshakeDone = func(state utls.ConnectionState, err error) {
				trace.TLSHandshakeDone(tls.ConnectionState{Version: state.Version, HandshakeComplete: state.HandshakeComplete, DidResume: state.DidResume, CipherSuite: state.CipherSuite, NegotiatedProtocol: state.NegotiatedProtocol, ServerName: state.ServerName, PeerCertificates: state.PeerCertificates, VerifiedChains: state.VerifiedChains}, err)
			}
		}
		if trace.DNSStart != nil {
			bridge.DNSStart = func(info fhttptrace.DNSStartInfo) { trace.DNSStart(httptrace.DNSStartInfo{Host: info.Host}) }
		}
		if trace.DNSDone != nil {
			bridge.DNSDone = func(info fhttptrace.DNSDoneInfo) {
				trace.DNSDone(httptrace.DNSDoneInfo{Addrs: info.Addrs, Err: info.Err, Coalesced: info.Coalesced})
			}
		}
		if trace.GotConn != nil {
			bridge.GotConn = func(info fhttptrace.GotConnInfo) {
				trace.GotConn(httptrace.GotConnInfo{Conn: info.Conn, Reused: info.Reused, WasIdle: info.WasIdle, IdleTime: info.IdleTime})
			}
		}
		if trace.WroteRequest != nil {
			bridge.WroteRequest = func(info fhttptrace.WroteRequestInfo) { trace.WroteRequest(httptrace.WroteRequestInfo{Err: info.Err}) }
		}
		if trace.WroteHeaders != nil {
			bridge.WroteHeaders = trace.WroteHeaders
		}
		if trace.GotFirstResponseByte != nil {
			bridge.GotFirstResponseByte = trace.GotFirstResponseByte
		}
		ctx = fhttptrace.WithClientTrace(ctx, bridge)
	}
	result, err := fhttp.NewRequestWithContext(ctx, request.Method, request.URL.String(), body)
	if err != nil {
		return nil, err
	}
	result.ContentLength = request.ContentLength
	result.TransferEncoding = append([]string(nil), request.TransferEncoding...)
	result.Close = request.Close
	if request.Host != "" {
		result.Host = request.Host
	}
	if request.GetBody != nil {
		result.GetBody = request.GetBody
	}
	result.Trailer = fhttp.Header(request.Trailer.Clone())
	for name, values := range request.Header {
		for _, value := range values {
			result.Header.Add(name, value)
		}
	}
	return result, nil
}

// WebSocket ownership transfers from the handshake to the real connection.
// Its Close and caller cancellation release the request slot exactly once.
type completionConn struct {
	net.Conn
	stop   func() bool
	finish func()
	once   sync.Once
}

func (c *completionConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.stop(); c.finish() })
	return err
}
func completionDial(ctx context.Context, dial browsertransport.DialContextFunc, finish func()) browsertransport.DialContextFunc {
	return func(dialCtx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(dialCtx, network, address)
		if err != nil {
			return nil, err
		}
		idle := netbudget.MarkActive(conn)
		done := func() { idle(); finish() }
		var once sync.Once
		complete := func() { once.Do(done) }
		stop := context.AfterFunc(ctx, func() { _ = conn.Close(); complete() })
		return &completionConn{Conn: conn, stop: stop, finish: complete}, nil
	}
}
