package proxydial

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func proxyServer(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		serve(conn)
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return listener.Addr().String()
}

func TestSOCKS4FragmentedReplyAndIdentity(t *testing.T) {
	address := proxyServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		var header [8]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			t.Error(err)
			return
		}
		user, _ := r.ReadString(0)
		host, _ := r.ReadString(0)
		if header[0] != 4 || header[1] != 1 || binary.BigEndian.Uint16(header[2:4]) != 443 || user != "account-42\x00" || host != "origin.invalid\x00" {
			t.Errorf("invalid SOCKS4a request: %v user=%q host=%q", header, user, host)
		}
		for _, b := range []byte{0, 90, 0, 0, 0, 0, 0, 0} {
			_, _ = conn.Write([]byte{b})
			time.Sleep(time.Millisecond)
		}
		_, _ = conn.Write([]byte("ok"))
	})
	d, err := New("socks4a://account-42@" + address)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.DialContext(context.Background(), "tcp", "origin.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "ok" {
		t.Fatalf("tunnel: %q, %v", got, err)
	}
}

func TestHTTPConnectPreservesBufferedTunnelBytes(t *testing.T) {
	address := proxyServer(t, func(conn net.Conn) {
		r, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			t.Error(err)
			return
		}
		if r.Method != "CONNECT" || r.Host != "origin.invalid:443" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Errorf("bad CONNECT: %+v", r)
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\nprefix")
		var b [1]byte
		if _, err := conn.Read(b[:]); err == nil {
			_, _ = io.WriteString(conn, "suffix")
		}
	})
	d, _ := New("http://user:pass@" + address)
	d.timeout = 100 * time.Millisecond
	conn, err := d.DialContext(context.Background(), "tcp", "origin.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The handshake deadline must not become the lifetime of a healthy tunnel.
	time.Sleep(150 * time.Millisecond)
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "prefixsuffix" {
		t.Fatalf("tunnel bytes=%q err=%v", got, err)
	}
}

func TestProxyCancellationClosesHandshakeSocket(t *testing.T) {
	for _, scheme := range []string{"http", "https", "socks4a", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			accepted := make(chan struct{})
			closed := make(chan struct{})
			address := proxyServer(t, func(conn net.Conn) {
				close(accepted)
				_, _ = io.Copy(io.Discard, conn)
				close(closed)
			})
			d, _ := New(scheme + "://" + address)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				conn, err := d.DialContext(ctx, "tcp", "origin.invalid:443")
				if conn != nil {
					_ = conn.Close()
				}
				done <- err
			}()
			<-accepted
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled handshake did not return")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("canceled handshake socket leaked")
			}
		})
	}
}

func TestProxyHandshakeHasIndependentTimeout(t *testing.T) {
	address := proxyServer(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	d, _ := New("http://" + address)
	d.timeout = 50 * time.Millisecond
	_, err := d.DialContext(context.Background(), "tcp", "origin.invalid:443")
	if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "timeout")) {
		t.Fatalf("unbounded handshake: %v", err)
	}
}
