package egress

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// browserClient.Do 是浏览器租约的 requestClient 实现(出口请求的 fhttp 翻译层),
// 此前 0%——没有任何测试真实穿过 Do 完成一次 HTTP 往返。锁定:
// (1) 真实请求经 tls-client(直连)到 httptest 源站,方法/头/体保真;
// (2) gzip 解压响应(Uncompressed=true)剥除 Content-Encoding/Length 且
//
//	ContentLength=-1——下游不得二次解码,这是防重复解压的契约。
func TestBrowserClientDoRoundTrip(t *testing.T) {
	var seenMethod, seenHeader, seenBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenMethod, seenHeader, seenBody = r.Method, r.Header.Get("X-Probe"), string(body)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("upstream-ok"))
	}))
	t.Cleanup(server.Close)

	client, err := newBrowserClient("", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	if err != nil {
		t.Fatalf("newBrowserClient: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/echo", stringReader("probe-body"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Probe", "egress-do")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)

	if seenMethod != http.MethodPost || seenHeader != "egress-do" || seenBody != "probe-body" {
		t.Fatalf("upstream saw method=%q header=%q body=%q", seenMethod, seenHeader, seenBody)
	}
	if response.StatusCode != http.StatusOK || string(payload) != "upstream-ok" {
		t.Fatalf("response = %d %q", response.StatusCode, payload)
	}
	if response.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("response header lost: %v", response.Header)
	}
}

// Uncompressed 响应翻译契约:tls-client 原地完成 gzip 解压后,Content-Encoding
// 与 Content-Length 必须剥除、ContentLength=-1——否则下游(标准 transport 链)
// 会按残留头再次尝试解码已解压的字节流。真实 gzip 源站经 Do 往返验证。
func TestBrowserClientUncompressedTranslation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		writer.Write([]byte("compressed-payload"))
		writer.Close()
	}))
	t.Cleanup(server.Close)

	client, err := newBrowserClient("", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)

	response, err := client.Do(mustRequest(t, server.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)

	if response.Header.Get("Content-Encoding") != "" {
		t.Fatal("Uncompressed 响应必须剥除 Content-Encoding(防下游二次解码)")
	}
	if response.ContentLength != -1 {
		t.Fatalf("Uncompressed 响应 ContentLength = %d, want -1", response.ContentLength)
	}
	if string(payload) != "compressed-payload" {
		t.Fatalf("decoded payload = %q", payload)
	}
}

func mustRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	return request
}

func stringReader(value string) io.Reader { return strings.NewReader(value) }
