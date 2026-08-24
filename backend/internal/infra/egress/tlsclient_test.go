package egress

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// dialWebSocket 的池模式安全失败重试环此前只有 sticky_retry 的 HTTP 版
// 测试覆盖,WebSocket 拨号路径零覆盖。坏代理(连接拒绝,安全失败标记)
// 必须:重试至 proxyPoolRetryLimit 后如实返回错误,而不是死循环或吞错。
func TestDialWebSocketPoolModeRetriesBoundedOnSafeFailure(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	// 端口 1 (tcpmux) 恒拒绝连接 —— "connection refused" 是安全失败标记。
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "pool-ws", Enabled: true, ProxyPool: true, Health: 1,
		EncryptedProxyURL: encryptedProxy(t, cipher, "socks5://127.0.0.1:1"),
	}}
	manager := NewManager(repository, cipher)
	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "acct")
	if err != nil || lease == nil {
		t.Fatalf("web pool lease: lease=%v err=%v", lease, err)
	}
	defer lease.Release()
	if lease.browser == nil {
		t.Fatal("web lease must carry a browser transport for websocket dialing")
	}
	startedAt := time.Now()
	_, _, dialErr := lease.DialWebSocket(context.Background(), "wss://example.invalid", nil, 2*time.Second)
	if dialErr == nil {
		t.Fatal("dial through a dead proxy must fail")
	}
	// 每次尝试都是立即拒绝(无握手等待),界内重试总耗时应远小于逐次满超时。
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("bounded retry took %v; refusal must be immediate per attempt", elapsed)
	}
	if !safeProxyConnectionFailure(dialErr, nil) {
		t.Fatalf("surfaced error must stay a safe connection failure, got: %v", dialErr)
	}
}

func TestToFHTTPRequestPreservesRequestFraming(t *testing.T) {
	payload := []byte(`{"message":"hello"}`)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://grok.com/rest/test", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "grok.com"
	request.Header.Set("Content-Type", "application/json")

	converted, err := toFHTTPRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if converted.ContentLength != int64(len(payload)) || len(converted.TransferEncoding) != 0 {
		t.Fatalf("contentLength=%d transferEncoding=%v", converted.ContentLength, converted.TransferEncoding)
	}
	if converted.Host != request.Host || converted.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("host=%q headers=%v", converted.Host, converted.Header)
	}
	if converted.GetBody == nil {
		t.Fatal("GetBody was not preserved")
	}
	body, err := converted.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("body=%q err=%v", got, err)
	}
}

func TestFromFHTTPResponseNormalizesAutoDecompressedHeaders(t *testing.T) {
	response := fromFHTTPResponse(&fhttp.Response{
		Status: "200 OK", StatusCode: http.StatusOK, Proto: "HTTP/2.0", ProtoMajor: 2,
		Header: fhttp.Header{
			"Content-Encoding": []string{"gzip"},
			"Content-Length":   []string{"128"},
			"Content-Type":     []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"status":"completed"}`)), ContentLength: 128, Uncompressed: true,
	})
	if response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Length") != "" {
		t.Fatalf("decoded response headers = %#v", response.Header)
	}
	if response.ContentLength != -1 || !response.Uncompressed {
		t.Fatalf("contentLength=%d uncompressed=%v", response.ContentLength, response.Uncompressed)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(data, []byte(`{"status":"completed"}`)) {
		t.Fatalf("body=%q err=%v", data, err)
	}
}

func TestFromFHTTPResponsePreservesCompressedHeaders(t *testing.T) {
	response := fromFHTTPResponse(&fhttp.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Header: fhttp.Header{"Content-Encoding": []string{"gzip"}, "Content-Length": []string{"128"}},
		Body:   io.NopCloser(bytes.NewReader(nil)), ContentLength: 128,
	})
	if response.Header.Get("Content-Encoding") != "gzip" || response.Header.Get("Content-Length") != "128" || response.ContentLength != 128 {
		t.Fatalf("compressed response = headers=%#v contentLength=%d", response.Header, response.ContentLength)
	}
}

func TestFromFHTTPResponseOwnsHeadersAndPreservesDeferredTrailers(t *testing.T) {
	source := &fhttp.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Header:           fhttp.Header{"X-Upstream": []string{"original"}},
		Trailer:          fhttp.Header{"X-Usage": nil},
		TransferEncoding: []string{"chunked"},
		Body:             io.NopCloser(bytes.NewReader(nil)),
	}
	response := fromFHTTPResponse(source)

	source.Header.Set("X-Upstream", "mutated")
	source.TransferEncoding[0] = "identity"
	// fhttp 会在读取 Body 到 EOF 时以这种方式填充已声明的 Trailer。
	source.Trailer.Set("X-Usage", "42")

	if response.Header.Get("X-Upstream") != "original" {
		t.Fatalf("response header aliases fhttp header: %#v", response.Header)
	}
	if len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" {
		t.Fatalf("response transfer encoding aliases fhttp value: %#v", response.TransferEncoding)
	}
	if response.Trailer.Get("X-Usage") != "42" {
		t.Fatalf("deferred trailer was lost: %#v", response.Trailer)
	}
}
