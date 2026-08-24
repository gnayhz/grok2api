package egress

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// scriptedDialer 不实现 ContextDialer——强制走 dialContext 的桥接路径。
type scriptedDialer struct {
	delay     time.Duration
	connected atomic.Int32 // 实际完成拨号的次数
}

func (d *scriptedDialer) Dial(network, address string) (net.Conn, error) {
	time.Sleep(d.delay)
	d.connected.Add(1)
	return &net.TCPConn{}, nil // 占位连接:仅验证关闭路径被调用
}

// closeTrackingConn 记录 Close 调用。
type closeTrackingConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeTrackingConn) Close() error { c.closed.Store(true); return nil }

// lateDialer 模拟"取消后迟到的拨号完成"——连接必须被回收关闭,不得泄漏 FD。
type lateDialer struct {
	release chan struct{}
	conn    *closeTrackingConn
}

func (d *lateDialer) Dial(network, address string) (net.Conn, error) {
	<-d.release
	return d.conn, nil
}

// SOCKS 桥接路径的取消语义:取消立即返回 ctx.Err()(不被拨号阻塞),
// 迟到的连接被异步关闭(FD 不泄漏)。
func TestDialContextCancellationBridgesAndReaps(t *testing.T) {
	// (1) 取消及时返回。
	dial := dialContext(&scriptedDialer{delay: 30 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := dial(ctx, "tcp", "10.0.0.1:8080"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled dial err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation blocked for %v", elapsed)
	}

	// (2) 迟到完成的连接被关闭。
	late := &lateDialer{release: make(chan struct{}), conn: &closeTrackingConn{}}
	lateDial := dialContext(late)
	lateCtx, lateCancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := lateDial(lateCtx, "tcp", "10.0.0.2:8080")
		result <- err
	}()
	time.Sleep(20 * time.Millisecond)
	lateCancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("late dial err = %v, want Canceled", err)
	}
	close(late.release) // 迟到的拨号现在完成
	deadline := time.Now().Add(2 * time.Second)
	for !late.conn.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("late connection was never reaped/closed — FD leak")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 实现 ContextDialer 的拨号器走直通路径(不经 goroutine 桥接)。
type directContextDialer struct {
	dialed atomic.Bool
}

func (d *directContextDialer) Dial(network, address string) (net.Conn, error) {
	d.dialed.Store(true)
	return nil, errors.New("should use DialContext")
}

func (d *directContextDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("context dial reached")
}

func TestDialContextPrefersContextDialer(t *testing.T) {
	dialer := &directContextDialer{}
	var bridged xproxy.Dialer = dialer
	dial := dialContext(bridged)
	_, err := dial(context.Background(), "tcp", "10.0.0.3:8080")
	if err == nil || err.Error() != "context dial reached" {
		t.Fatalf("ContextDialer not preferred: %v", err)
	}
	if dialer.dialed.Load() {
		t.Fatal("plain Dial was used despite ContextDialer being available")
	}
}
