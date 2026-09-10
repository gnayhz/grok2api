package tunnelproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Asutorufa/yuhaiin/pkg/net/netapi"
)

type blockedProtocolProxy struct {
	netapi.EmptyDispatch
	conn    net.Conn
	entered chan struct{}
	write   bool
}

func (*blockedProtocolProxy) PacketConn(context.Context, netapi.Address) (net.PacketConn, error) {
	return nil, errors.New("UDP unsupported")
}

func (p *blockedProtocolProxy) Conn(ctx context.Context, _ netapi.Address) (net.Conn, error) {
	if err := ownHandshakeConn(ctx, p.conn); err != nil {
		return nil, err
	}
	close(p.entered)
	if p.write {
		if _, err := p.conn.Write([]byte("protocol-header")); err != nil {
			return nil, err
		}
	}
	return p.conn, nil
}

func TestCanceledProtocolWriteClosesRawSocket(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	proxy := &blockedProtocolProxy{conn: conn, entered: make(chan struct{}), write: true}
	d := &Dialer{proxy: proxy}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := d.DialContext(ctx, "tcp", "origin.invalid:443"); done <- err }()
	<-proxy.entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("protocol write survived cancellation")
	}
	var b [1]byte
	if _, err := peer.Read(b[:]); err != io.EOF {
		t.Fatalf("raw socket not closed: %v", err)
	}
}

func TestCompletedTunnelOutlivesDialContext(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	d := &Dialer{proxy: &blockedProtocolProxy{conn: conn, entered: make(chan struct{})}}
	ctx, cancel := context.WithCancel(context.Background())
	result, err := d.DialContext(ctx, "tcp", "origin.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	cancel()
	done := make(chan error, 1)
	go func() { _, err := peer.Write([]byte("live")); done <- err }()
	var data [4]byte
	if _, err := io.ReadFull(result, data[:]); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if string(data[:]) != "live" {
		t.Fatal("tunnel data corrupted")
	}
}
