package tunnelproxy

import (
	"context"
	"net"
	"time"
)

type handshakeOwnerKey struct{}

// handshakeOwner retains the raw socket even if a protocol wrapper fails
// without returning it. It also bounds initial protocol writes, not just TLS.
// Keep the original connection type intact for VLESS Vision's TLS inspection.
type handshakeOwner struct {
	conn net.Conn
	stop func() bool
	done chan struct{}
}

func ownHandshakeConn(ctx context.Context, conn net.Conn) error {
	owner, _ := ctx.Value(handshakeOwnerKey{}).(*handshakeOwner)
	if owner == nil {
		return nil
	}
	owner.conn = conn
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	owner.done = make(chan struct{})
	owner.stop = context.AfterFunc(ctx, func() { _ = conn.Close(); close(owner.done) })
	return nil
}

func (o *handshakeOwner) finish(ctx context.Context, result net.Conn, err error) (net.Conn, error) {
	if o.stop != nil && !o.stop() {
		<-o.done
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && o.conn != nil {
		err = o.conn.SetDeadline(time.Time{})
	}
	if err != nil {
		if result != nil {
			_ = result.Close()
		}
		if o.conn != nil {
			_ = o.conn.Close()
		}
		return nil, err
	}
	return result, nil
}
