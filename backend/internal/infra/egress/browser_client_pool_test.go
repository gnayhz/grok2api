package egress

import (
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// acceptCountingListener 只累计源站接受的 TCP 连接总数(与生命周期测试的
// countingListener 的活跃差值语义不同:池保留行为要看累计接受数)。
type acceptCountingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *acceptCountingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	return conn, nil
}

// TestBrowserClientConnectionPoolReuse 锁定连接池契约:newBrowserClient
// 显式设置 MaxIdleConnsPerHost=64。回归态(tls-client 不传 TransportOptions,
// 落到 Go 默认 MaxIdleConnsPerHost=2)时第一轮并发结束后只有 2 条连接被
// 空闲池保留,第二轮必须重开其余连接——累计接受连接数显著上升。
// 处理器刻意加延迟,保证 8 个并发请求真实同时在建连。
func TestBrowserClientConnectionPoolReuse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted := &acceptCountingListener{Listener: listener}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 慢响应:8 个并发请求全部处于在建状态,迫使客户端为每个请求
		// 各持一条连接(而不是顺序快速复用少量连接)。
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("pool-ok"))
	})}
	go func() { _ = server.Serve(accepted) }()
	t.Cleanup(func() { _ = server.Close() })
	baseURL := "http://" + listener.Addr().String()

	client, err := newBrowserClient("", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	if err != nil {
		t.Fatalf("newBrowserClient: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)

	runBurst := func(label string) {
		t.Helper()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				response, err := client.Do(mustRequest(t, baseURL))
				if err != nil {
					t.Errorf("%s burst Do: %v", label, err)
					return
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}()
		}
		wg.Wait()
	}

	runBurst("first")
	// 等待连接归还空闲池(Transport 在响应体读尽后异步归还并裁剪超额)。
	time.Sleep(300 * time.Millisecond)
	before := accepted.accepted.Load()
	runBurst("second")
	opened := accepted.accepted.Load() - before
	t.Logf("first-burst accepted=%d second-burst opened=%d", before, opened)
	// 池上限 64 时第二轮应几乎零新建(容忍少量时序性新建);
	// 默认 2 条上限的回归会重开 ≥6 条。
	if opened > 2 {
		t.Fatalf("second burst opened %d new connections; idle pool should retain the first burst's 8 connections (MaxIdleConnsPerHost=64)", opened)
	}
}
