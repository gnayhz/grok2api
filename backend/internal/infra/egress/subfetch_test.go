package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/netguard"
)

func plainNormalize(value string) (string, error) { return value, nil }

type countingOwner struct{ calls atomic.Int64 }

func (o *countingOwner) ManageHTTPTransport(_ context.Context, transport *http.Transport) (http.RoundTripper, func(), error) {
	o.calls.Add(1)
	return transport, func() {}, nil
}

func TestSubscriptionFetcherUsesClashUserAgent(t *testing.T) {
	var userAgent string
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		userAgent = request.Header.Get("User-Agent")
		_, _ = writer.Write([]byte("http://proxy.example:8080"))
	}))
	defer proxy.Close()

	fetcher := NewSubscriptionFetcher(nil, plainNormalize)
	body, err := fetcher.FetchProxySubscription(context.Background(), "http://1.1.1.1/subscription", proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	if userAgent != "Clash.Meta" || !strings.Contains(string(body), "proxy.example") {
		t.Fatalf("User-Agent=%q body=%q", userAgent, body)
	}
}

func TestSubscriptionFetcherUsesManagedTransport(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		if request.URL.Host != "1.1.1.1" {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = writer.Write([]byte("http://proxy.example:8080"))
	}))
	defer server.Close()

	owner := &countingOwner{}
	fetcher := NewSubscriptionFetcher(owner, plainNormalize)
	// A proxied fetch routes through the configured (local) proxy; the
	// managed runtime must own the transport that performs it.
	if _, err := fetcher.FetchProxySubscription(context.Background(), "http://1.1.1.1/subscription", server.URL); err != nil {
		t.Fatal(err)
	}
	if owner.calls.Load() != 1 || hits.Load() != 1 {
		t.Fatalf("ownership=%d actual requests=%d", owner.calls.Load(), hits.Load())
	}
}

// An unproxied fetch to a private destination must be rejected before any
// request: the SSRF narrowing lives in the dial path.
func TestSubscriptionFetcherRejectsPrivateUnproxiedTarget(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
	}))
	defer server.Close()
	fetcher := NewSubscriptionFetcher(nil, plainNormalize)
	if _, err := fetcher.FetchProxySubscription(context.Background(), server.URL, ""); err == nil {
		t.Fatal("private subscription target accepted")
	}
	if hits.Load() != 0 {
		t.Fatalf("private target reached the server: %d", hits.Load())
	}
}

// TestSubscriptionTargetEnforcesSharedPublicAddressPolicy 锚定订阅抓取不再
// 自带弱化网段表:重定向与订阅目标曾用一份只覆盖到 2001:db8::/32 的本地
// 前缀清单，使 NAT64/6to4/Teredo 等可封装内网的特殊用途地址通过校验，而
// 媒体导入与 Web 附件早就拒绝它们。现在订阅路径直接使用 netguard。
func TestSubscriptionTargetEnforcesSharedPublicAddressPolicy(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.1", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.10.1",
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1",
		"::1", "fc00::1", "2001:db8::1", "::ffff:127.0.0.1",
		// 以下为本地旧清单遗漏、netguard 覆盖的特殊用途网段。
		"64:ff9b::a00:1", "64:ff9b:1::a00:1", "100::1", "2001::1",
		"2002:a00:1::1", "3fff::1", "5f00::1",
	} {
		address := netip.MustParseAddr(raw)
		if err := validatePublicSubscriptionTarget(context.Background(), "http://["+raw+"]/subscription"); err == nil {
			t.Errorf("non-public subscription target accepted: %s", raw)
		}
		if netguard.IsPublicAddress(address) {
			t.Errorf("netguard accepts non-public address: %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !netguard.IsPublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("netguard rejects public address: %s", raw)
		}
	}
}

func TestValidatePublicSubscriptionTargetRejectsPrivateAddresses(t *testing.T) {
	for _, value := range []string{
		"http://127.0.0.1/subscription",
		"http://10.0.0.1/subscription",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/subscription",
	} {
		if err := validatePublicSubscriptionTarget(context.Background(), value); err == nil {
			t.Fatalf("private subscription target accepted: %s", value)
		}
	}
	for _, value := range []string{"https://1.1.1.1/subscription", "https://[2606:4700:4700::1111]/subscription"} {
		if err := validatePublicSubscriptionTarget(context.Background(), value); err != nil {
			t.Fatalf("public subscription target rejected: %s: %v", value, err)
		}
	}
}

func TestSubscriptionSOCKSCancellationClosesRealHandshakeSocket(t *testing.T) {
	for _, scheme := range []string{"socks4a", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			entered, closed := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(closed)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var greeting [32]byte
				if _, err = conn.Read(greeting[:]); err != nil {
					return
				}
				close(entered)
				_, _ = io.Copy(io.Discard, conn)
			}()
			transport, err := subscriptionTransport(scheme + "://" + listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer transport.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				conn, err := transport.DialContext(ctx, "tcp", "example.com:443")
				if conn != nil {
					_ = conn.Close()
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("handshake did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error: %v", err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("SOCKS caller stayed blocked")
			}
			select {
			case <-closed:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("canceled SOCKS socket remains open")
			}
		})
	}
}

func TestSubscriptionTransportSupportsConfiguredProxyProtocols(t *testing.T) {
	for _, proxyURL := range []string{
		"http://127.0.0.1:8080",
		"https://127.0.0.1:8443",
		"socks4://127.0.0.1:1080",
		"socks4a://127.0.0.1:1080",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
	} {
		transport, err := subscriptionTransport(proxyURL)
		if err != nil {
			t.Fatalf("proxy %s: %v", proxyURL, err)
		}
		transport.CloseIdleConnections()
	}
}
