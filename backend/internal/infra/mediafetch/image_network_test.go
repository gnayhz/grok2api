package mediafetch

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

type imageResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (fn imageResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return fn(ctx, network, host)
}

type imageNetworkFixture struct {
	source                      *ImageSource
	calls, lookups, connections atomic.Int32
}

// Keep the real verified TLS/HTTP transport, body and connection ownership.
// The test maps the validated public endpoint onto its local TLS listener.
func newImageNetworkFixture(t *testing.T, handle func(http.ResponseWriter, *http.Request)) *imageNetworkFixture {
	t.Helper()
	fx := &imageNetworkFixture{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.calls.Add(1)
		if r.Host != "example.com" || r.Header.Get("Accept") != "image/*" || r.Header.Get("User-Agent") != "grok2api-media-importer/1.0" {
			t.Error("image fetch lost original Host or request headers")
		}
		handle(w, r)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			fx.connections.Add(1)
		}
		if state == http.StateClosed {
			fx.connections.Add(-1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	if err := server.Certificate().VerifyHostname("example.com"); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	fx.source = NewImageSource()
	fx.source.resolver = imageResolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		fx.lookups.Add(1)
		if network != "ip" || host != "example.com" {
			t.Errorf("lookup=%s %s", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	fx.source.newClient = func(target *importTarget) (*http.Client, *http.Transport) {
		client, transport := newIngestHTTPClient(target)
		transport.TLSClientConfig.RootCAs = roots
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "93.184.216.34:443" {
				return nil, fmt.Errorf("unexpected unpinned destination %s", address)
			}
			if err := ssrfSafeControl(network, address, nil); err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		return client, transport
	}
	return fx
}
func (fx *imageNetworkFixture) assertClosed(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for fx.connections.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("fetch returned with %d open connections", fx.connections.Load())
		}
		time.Sleep(time.Millisecond)
	}
}
func imageNetworkBytes(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func imageNetworkURL(t *testing.T, path string) *url.URL {
	t.Helper()
	parsed, err := mediadomain.ParseInputImageURL("https://example.com" + path)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestImageSourceOwnsRealNetworkFailureAndRedirectResources(t *testing.T) {
	for _, scenario := range []string{"success", "relative_redirect", "private_redirect", "redirect_rebinding", "redirect_limit", "declared_oversize", "streamed_oversize", "truncated_body", "upstream_rejected", "cancel_body", "dns_failure", "cancel_dns"} {
		t.Run(scenario, func(t *testing.T) {
			picture := imageNetworkBytes(t)
			entered := make(chan struct{}, 1)
			fx := newImageNetworkFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "relative_redirect":
					if r.URL.Path == "/image" {
						http.Redirect(w, r, "/final?size=1", http.StatusFound)
						return
					}
				case "private_redirect":
					http.Redirect(w, r, "https://127.0.0.1/private", http.StatusFound)
					return
				case "redirect_rebinding", "redirect_limit":
					http.Redirect(w, r, "/next", http.StatusTemporaryRedirect)
					return
				case "declared_oversize":
					w.Header().Set("Content-Length", fmt.Sprint(len(picture)+1))
					_, _ = w.Write(append(picture, 1))
					return
				case "streamed_oversize":
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					_, _ = w.Write(append(picture, 1))
					return
				case "truncated_body":
					w.Header().Set("Content-Length", fmt.Sprint(len(picture)))
					_, _ = w.Write(picture[:len(picture)-1])
					return
				case "upstream_rejected":
					http.Error(w, "not available", http.StatusServiceUnavailable)
					return
				case "cancel_body":
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					entered <- struct{}{}
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(picture)
			})
			cause := errors.New("test DNS unavailable")
			if scenario == "dns_failure" {
				fx.source.resolver = imageResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, cause })
			}
			if scenario == "cancel_dns" {
				fx.source.resolver = imageResolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
					entered <- struct{}{}
					<-ctx.Done()
					return nil, ctx.Err()
				})
			}
			if scenario == "redirect_rebinding" {
				fx.source.resolver = imageResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
					if fx.lookups.Add(1) == 1 {
						return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
					}
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type result struct {
				data []byte
				err  error
			}
			done := make(chan result, 1)
			target := imageNetworkURL(t, "/image")
			go func() {
				data, err := fx.source.FetchImage(ctx, target, int64(len(picture)))
				done <- result{data, err}
			}()
			if scenario == "cancel_body" || scenario == "cancel_dns" {
				select {
				case <-entered:
					cancel()
				case <-ctx.Done():
					t.Fatal("operation never reached cancellation boundary")
				}
			}
			got := <-done
			switch scenario {
			case "success", "relative_redirect":
				if got.err != nil || !bytes.Equal(got.data, picture) {
					t.Fatalf("fetch returned wrong bytes: %v", got.err)
				}
			case "private_redirect", "redirect_rebinding":
				if !errors.Is(got.err, mediadomain.ErrInputImageURLBlocked) {
					t.Fatalf("unsafe redirect=%v", got.err)
				}
			case "declared_oversize", "streamed_oversize":
				if !errors.Is(got.err, mediadomain.ErrInputImageTooLarge) {
					t.Fatalf("oversize=%v", got.err)
				}
			case "dns_failure":
				if !errors.Is(got.err, cause) || errors.Is(got.err, mediadomain.ErrInputImageURLBlocked) {
					t.Fatalf("DNS classification=%v", got.err)
				}
			case "cancel_body", "cancel_dns":
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("cancellation=%v", got.err)
				}
			default:
				if got.err == nil {
					t.Fatalf("failed %s fetch accepted", scenario)
				}
			}
			if got.err != nil && len(got.data) != 0 {
				t.Fatal("failed fetch delivered partial input bytes")
			}
			if scenario == "relative_redirect" && (fx.lookups.Load() != 2 || fx.calls.Load() != 2) {
				t.Fatalf("redirect was not revalidated: lookups=%d calls=%d", fx.lookups.Load(), fx.calls.Load())
			}
			if (scenario == "private_redirect" || scenario == "redirect_rebinding") && fx.calls.Load() != 1 {
				t.Fatalf("blocked redirect made extra network calls=%d", fx.calls.Load())
			}
			if scenario == "redirect_limit" && fx.calls.Load() != 6 {
				t.Fatalf("redirect bound changed: calls=%d", fx.calls.Load())
			}
			if (scenario == "dns_failure" || scenario == "cancel_dns") && fx.calls.Load() != 0 {
				t.Fatal("failed resolution reached network")
			}
			fx.assertClosed(t)
		})
	}
}
