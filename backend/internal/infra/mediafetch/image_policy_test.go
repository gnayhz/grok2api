package mediafetch

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"testing"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

type importResolverFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

func (fn importResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return fn(ctx, network, host)
}

func TestPublicIngestAddressPolicy(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.1",
		"192.0.2.1", "198.18.0.1", "240.0.0.1", "::1", "fd00::1", "2001:db8::1",
	}
	for _, raw := range blocked {
		if isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("address %s was allowed", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("public address %s was blocked", raw)
		}
	}
}

func TestValidRedirectURLRejectsCredentialsAndUnexpectedPorts(t *testing.T) {
	for _, raw := range []string{"file:///tmp/a.png", "https://user:pass@example.com/a.png", "https://example.com:8443/a.png"} {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err == nil && mediadomain.AllowedInputImageURL(request.URL) {
			t.Errorf("URL %q was allowed", raw)
		}
	}
	request, err := http.NewRequest(http.MethodGet, "https://example.com/a.png", nil)
	if err != nil || !mediadomain.AllowedInputImageURL(request.URL) {
		t.Fatalf("public HTTPS URL rejected: %v", err)
	}
}

func TestResolveImportTargetPinsValidatedPublicAddress(t *testing.T) {
	parsed, err := url.Parse("https://Images.Example.test/photo.png?size=large#ignored")
	if err != nil {
		t.Fatal(err)
	}
	resolver := importResolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "images.example.test" {
			t.Fatalf("lookup network=%q host=%q", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	target, err := resolveImportTarget(context.Background(), parsed, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if target.fetchURL.Host != "93.184.216.34:443" || target.fetchURL.Path != "/photo.png" || target.fetchURL.RawQuery != "size=large" || target.fetchURL.Fragment != "" {
		t.Fatalf("fetch URL = %s", target.fetchURL)
	}
	if target.hostHeader != "Images.Example.test" || target.serverName != "images.example.test" {
		t.Fatalf("host header=%q server name=%q", target.hostHeader, target.serverName)
	}
	client, transport := newIngestHTTPClient(target)
	defer transport.CloseIdleConnections()
	if client.CheckRedirect == nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.ServerName != "images.example.test" || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("pinned HTTPS client did not preserve strict TLS verification")
	}
}

func TestResolveImportTargetRejectsAnyNonPublicDNSAnswer(t *testing.T) {
	parsed, err := url.Parse("https://images.example.test/photo.png")
	if err != nil {
		t.Fatal(err)
	}
	resolver := importResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}, nil
	})
	if _, err := resolveImportTarget(context.Background(), parsed, resolver); !errors.Is(err, mediadomain.ErrInputImageURLBlocked) {
		t.Fatalf("mixed public/private DNS answer was not blocked: %v", err)
	}
}

func TestResolveImportTargetDoesNotClassifyDNSFailureAsPolicyBlock(t *testing.T) {
	parsed, err := url.Parse("https://missing.example.test/photo.png")
	if err != nil {
		t.Fatal(err)
	}
	lookupErr := errors.New("temporary DNS failure")
	resolver := importResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, lookupErr
	})
	_, err = resolveImportTarget(context.Background(), parsed, resolver)
	if !errors.Is(err, lookupErr) || errors.Is(err, mediadomain.ErrInputImageURLBlocked) {
		t.Fatalf("DNS failure classification = %v", err)
	}
}
