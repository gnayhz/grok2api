// Package proxydial owns the lifetime of TCP proxy handshakes. A request
// deadline bounds CONNECT/authentication as well as opening the proxy socket.
package proxydial

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const HandshakeTimeout = 10 * time.Second

type Dialer struct {
	proxy   *url.URL
	direct  net.Dialer
	timeout time.Duration
}

func New(proxyURL string) (*Dialer, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("invalid proxy address")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			port = "1080"
		}
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	switch u.Scheme {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	return &Dialer{proxy: u, direct: net.Dialer{Timeout: HandshakeTimeout, KeepAlive: 30 * time.Second}, timeout: HandshakeTimeout}, nil
}

func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("proxy requires TCP")
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	if strings.HasPrefix(d.proxy.Scheme, "socks5") {
		u := *d.proxy
		u.Scheme = "socks5"
		dialer, err := xproxy.FromURL(&u, &d.direct)
		if err != nil {
			return nil, err
		}
		conn, err := dialer.(xproxy.ContextDialer).DialContext(ctx, network, address)
		if ctx.Err() != nil {
			if conn != nil {
				_ = conn.Close()
			}
			return nil, ctx.Err()
		}
		return conn, err
	}
	conn, err := d.direct.DialContext(ctx, network, d.proxy.Host)
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Cancel the actual socket; returning from a goroutine wrapper alone would
	// leave the handshake and FD alive when a proxy never responds.
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); close(stopped) })
	result, err := d.handshake(ctx, conn, address)
	if !stop() {
		<-stopped
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxyconnect: %w", err)
	}
	if err := result.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return result, nil
}

func (d *Dialer) handshake(ctx context.Context, conn net.Conn, address string) (net.Conn, error) {
	if d.proxy.Scheme == "socks4" || d.proxy.Scheme == "socks4a" {
		return d.socks4(ctx, conn, address)
	}
	if d.proxy.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: d.proxy.Hostname(), MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = tlsConn
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: address}, Host: address, Header: make(http.Header)}
	if d.proxy.User != nil {
		password, _ := d.proxy.User.Password()
		credentials := d.proxy.User.Username() + ":" + password
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	if err := request.Write(conn); err != nil {
		return nil, err
	}
	// Bound response headers without discarding tunnel bytes buffered with the
	// CONNECT response. Successful CONNECT has no HTTP response body to drain.
	limited := &io.LimitedReader{R: conn, N: 64 << 10}
	reader := bufio.NewReader(limited)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy returned HTTP %d", response.StatusCode)
	}
	buffered := make([]byte, reader.Buffered())
	if _, err := io.ReadFull(reader, buffered); err != nil {
		return nil, err
	}
	if len(buffered) == 0 {
		return conn, nil
	}
	return &bufferedConn{Conn: conn, prefix: buffered}, nil
}

type bufferedConn struct {
	net.Conn
	prefix []byte
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if len(c.prefix) != 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

func (d *Dialer) socks4(ctx context.Context, conn net.Conn, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid SOCKS destination port")
	}
	ip := net.ParseIP(host).To4()
	remoteDNS := d.proxy.Scheme == "socks4a" && net.ParseIP(host) == nil
	if ip == nil && !remoteDNS {
		addrs, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil {
			return nil, fmt.Errorf("resolve SOCKS4 destination: %w", err)
		}
		if len(addrs) == 0 {
			return nil, errors.New("SOCKS4 requires an IPv4 destination")
		}
		ip = addrs[0].To4()
	}
	if remoteDNS {
		ip = net.IPv4(0, 0, 0, 1).To4()
	}
	if ip == nil {
		return nil, errors.New("SOCKS4 requires IPv4")
	}
	user := ""
	if d.proxy.User != nil {
		user = d.proxy.User.Username()
	}
	if strings.ContainsRune(user, 0) || strings.ContainsRune(host, 0) {
		return nil, errors.New("invalid SOCKS4 user or host")
	}
	request := []byte{4, 1, 0, 0}
	binary.BigEndian.PutUint16(request[2:], uint16(port))
	request = append(request, ip...)
	request = append(request, user...)
	request = append(request, 0)
	if remoteDNS {
		request = append(request, host...)
		request = append(request, 0)
	}
	if _, err := conn.Write(request); err != nil {
		return nil, err
	}
	var reply [8]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return nil, err
	}
	if reply[0] != 0 || reply[1] != 90 {
		return nil, errors.New("SOCKS4 CONNECT rejected")
	}
	return conn, nil
}
