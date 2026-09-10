package browsertransport

import (
	"net"
	"sync/atomic"
	"time"

	tls "github.com/bogdanfinn/utls"
)

// fhttp can hold its stream mutex while flushing frames. A write deadline
// bounds an unresponsive peer even without request cancellation. Cancellation
// interrupts a write that stays blocked across two samples; normal RST_STREAM
// writes complete without closing healthy multiplexed streams.
const canceledWriteGrace = 25 * time.Millisecond

type h2WireConn struct {
	*tls.UConn
	raw         net.Conn
	timeout     time.Duration
	established atomic.Bool
	writing     atomic.Pointer[time.Time]
}

func newH2WireConn(conn *tls.UConn, timeout time.Duration) *h2WireConn {
	return &h2WireConn{UConn: conn, raw: conn.NetConn(), timeout: timeout}
}

func (c *h2WireConn) Write(p []byte) (int, error) {
	// The preface retains the complete establishment deadline until finish.
	if c.established.Load() {
		if err := c.raw.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	started := time.Now()
	c.writing.Store(&started)
	n, err := c.UConn.Write(p)
	c.writing.Store(nil)
	return n, err
}

// Close the socket directly: TLS close_notify may itself wait for the blocked
// TLS write lock. The peer cannot receive a graceful close on a stalled socket.
func (c *h2WireConn) Close() error { return c.raw.Close() }

func (a *h2Attempt) interruptCanceledWrite() {
	ticker := time.NewTicker(canceledWriteGrace)
	defer ticker.Stop()
	// Cancellation work is bounded even if a caller abandons an unread body.
	// The socket write deadline remains the final bound for later writes.
	deadline := time.NewTimer(4 * canceledWriteGrace)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return
		case <-a.done:
			return
		case <-ticker.C:
			c := a.conn.Load()
			if c == nil || c.wire == nil {
				continue
			}
			writing := c.wire.writing.Load()
			if writing != nil && time.Since(*writing) >= canceledWriteGrace {
				_ = c.raw.Close()
				return
			}
		}
	}
}
