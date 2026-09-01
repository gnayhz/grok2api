package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBrowserClientBoundsDeadlinelessRequests 锁定无 deadline 调用方的
// 兜底超时:tls-client 连接阶段无独立超时(拨号超时=整请求超时 7200s),
// 无 deadline 的请求挂在永不响应的服务上会被拖满。包装层给这类请求
// 附加上限。
func TestBrowserClientBoundsDeadlinelessRequests(t *testing.T) {
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-blocked
	}))
	t.Cleanup(func() {
		close(blocked)
		server.Close()
	})

	client, err := newBrowserClient("", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)

	original := browserClientDefaultRequestTimeout
	browserClientDefaultRequestTimeout = 300 * time.Millisecond
	t.Cleanup(func() { browserClientDefaultRequestTimeout = original })

	done := make(chan error, 1)
	go func() {
		request, requestErr := http.NewRequest(http.MethodGet, server.URL, nil)
		if requestErr != nil {
			done <- requestErr
			return
		}
		response, doErr := client.Do(request)
		if doErr == nil {
			_ = response.Body.Close()
			done <- &deadlineBoundError{}
			return
		}
		done <- doErr
	}()
	select {
	case err := <-done:
		if _, sentinel := err.(*deadlineBoundError); sentinel {
			t.Fatal("deadlineless request unexpectedly succeeded against a non-responsive server")
		}
		if err == nil || !isContextDeadline(err) {
			t.Fatalf("deadlineless request must fail with bounded deadline, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deadlineless request hung on a non-responsive server; wrapper bound is not applied")
	}
}

type deadlineBoundError struct{}

func (*deadlineBoundError) Error() string { return "unexpected success" }

func isContextDeadline(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "context deadline exceeded") || err.Error() == "Timeout")
}

// TestBrowserClientPreservesCallerDeadlineAndBodyStreams 锁定兼容契约:
// 调用方自带 deadline 时包装层不缩短其预算,且响应体在 Do 返回后仍可
// 完整读取(取消绑定在 body Close 上,不在 Do 出口)。
func TestBrowserClientPreservesCallerDeadlineAndBodyStreams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("streamed-body"))
	}))
	t.Cleanup(server.Close)

	client, err := newBrowserClient("", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("body read after Do: %v", err)
	}
	_ = response.Body.Close()
	if string(body) != "streamed-body" {
		t.Fatalf("body = %q", body)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("caller deadline must remain intact: %v", err)
	}
}
