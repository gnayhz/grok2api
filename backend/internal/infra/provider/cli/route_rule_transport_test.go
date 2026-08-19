package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// routeRuleEgressRepository serves fixed nodes plus an operations config.
type routeRuleEgressRepository struct {
	emptyEgressRepository
	nodes  map[uint64]domainegress.Node
	config domainegress.OperationsConfig
}

func (r routeRuleEgressRepository) GetEgressNode(_ context.Context, id uint64) (domainegress.Node, error) {
	if node, ok := r.nodes[id]; ok {
		return node, nil
	}
	return domainegress.Node{}, repository.ErrNotFound
}

func (r routeRuleEgressRepository) GetEgressOperationsConfig(context.Context) (domainegress.OperationsConfig, error) {
	return r.config, nil
}

// newRouteRuleProxyPair starts a plain HTTP proxy that counts forwarded
// requests and an origin server behind it.
func newRouteRuleProxyPair(t *testing.T) (proxyURL string, proxyCalls *int64, upstream *httptest.Server) {
	t.Helper()
	var counter int64
	upstream = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Origin", "upstream")
		_, _ = writer.Write([]byte("upstream-body"))
	}))
	t.Cleanup(upstream.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Host == "" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		forward, err := http.NewRequestWithContext(request.Context(), request.Method, request.URL.String(), request.Body)
		if err != nil {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		forward.Header = request.Header.Clone()
		response, err := http.DefaultTransport.RoundTrip(forward)
		if err != nil {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		writer.WriteHeader(response.StatusCode)
		_, _ = io.Copy(writer, response.Body)
		atomic.AddInt64(&counter, 1)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, &counter, upstream
}

func newRouteRuleTransport(t *testing.T, proxyURL string, rules []domainegress.RouteRule) *egressTransport {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	node := domainegress.Node{ID: 21, Name: "rule-exit", Scope: domainegress.ScopeBuild, Enabled: true, EncryptedProxyURL: encryptedProxy}
	repo := routeRuleEgressRepository{nodes: map[uint64]domainegress.Node{21: node}, config: domainegress.OperationsConfig{RouteRules: rules}}
	manager := infraegress.NewManager(repo, cipher)
	return &egressTransport{manager: manager, fallback: http.DefaultTransport}
}

// newRouteRuleTwoExitTransport builds a transport with a rule exit (node 21)
// and a separate binding exit (node 31) so assertions can tell which path a
// request actually took.
func newRouteRuleTwoExitTransport(t *testing.T, ruleProxyURL, bindingProxyURL string, rules []domainegress.RouteRule) *egressTransport {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	ruleProxy, err := cipher.Encrypt(ruleProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	bindingProxy, err := cipher.Encrypt(bindingProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[uint64]domainegress.Node{
		21: {ID: 21, Name: "rule-exit", Scope: domainegress.ScopeBuild, Enabled: true, EncryptedProxyURL: ruleProxy},
		31: {ID: 31, Name: "binding-exit", Scope: domainegress.ScopeBuild, Enabled: true, EncryptedProxyURL: bindingProxy},
	}
	repo := routeRuleEgressRepository{nodes: nodes, config: domainegress.OperationsConfig{RouteRules: rules}}
	manager := infraegress.NewManager(repo, cipher)
	return &egressTransport{manager: manager, fallback: http.DefaultTransport}
}

func TestEgressTransportRoutesByTrafficClassRule(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	transport := newRouteRuleTransport(t, proxyURL, []domainegress.RouteRule{
		{Scope: domainegress.ScopeBuild, Class: domainegress.TrafficClassBilling, TargetMode: domainegress.RouteRuleTargetFixed, TargetNodeID: 21, Enabled: true},
	})

	// Billing class honors the rule and exits through the fixed node's proxy.
	request, err := http.NewRequestWithContext(
		infraegress.WithTrafficClass(context.Background(), domainegress.TrafficClassBilling),
		http.MethodGet, upstream.URL+"/billing", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "upstream-body" {
		t.Fatalf("body = %q", body)
	}
	if got := atomic.LoadInt64(proxyCalls); got != 1 {
		t.Fatalf("proxy calls = %d, want 1 (rule must pin the exit)", got)
	}

	// Credential class has no rule: the empty node pool keeps the direct path.
	request, err = http.NewRequestWithContext(
		infraegress.WithTrafficClass(context.Background(), domainegress.TrafficClassCredential),
		http.MethodGet, upstream.URL+"/oauth2/token", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err = transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := atomic.LoadInt64(proxyCalls); got != 1 {
		t.Fatalf("proxy calls after unrouted class = %d, want unchanged", got)
	}
}

func TestEgressTransportKeepsBindingForInference(t *testing.T) {
	ruleProxyURL, ruleCalls, upstream := newRouteRuleProxyPair(t)
	bindingProxyURL, bindingCalls, _ := newRouteRuleProxyPair(t)
	transport := newRouteRuleTwoExitTransport(t, ruleProxyURL, bindingProxyURL, []domainegress.RouteRule{
		{Scope: domainegress.ScopeBuild, Class: domainegress.TrafficClassInference, TargetMode: domainegress.RouteRuleTargetFixed, TargetNodeID: 21, Enabled: true},
	})

	bound := infraegress.WithEgressNode(context.Background(), 31)
	request, err := http.NewRequestWithContext(bound, http.MethodGet, upstream.URL+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "upstream-body" {
		t.Fatalf("body = %q", body)
	}
	if got := atomic.LoadInt64(ruleCalls); got != 0 {
		t.Fatalf("rule exit calls = %d, want 0 (bound inference must not be rerouted)", got)
	}
	if got := atomic.LoadInt64(bindingCalls); got != 1 {
		t.Fatalf("binding exit calls = %d, want 1", got)
	}
}

func TestEgressTransportUnboundInferenceFollowsRule(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	transport := newRouteRuleTransport(t, proxyURL, []domainegress.RouteRule{
		{Scope: domainegress.ScopeBuild, Class: domainegress.TrafficClassInference, TargetMode: domainegress.RouteRuleTargetFixed, TargetNodeID: 21, Enabled: true},
	})

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := atomic.LoadInt64(proxyCalls); got != 1 {
		t.Fatalf("proxy calls = %d, want 1 (unbound inference follows the rule)", got)
	}
}

// A fixed rule whose target became unavailable must not fail the request:
// the transport falls back to the ordinary pool path and records the outcome.
func TestEgressTransportFallsBackWhenRuleTargetUnavailable(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	transport := newRouteRuleTransport(t, proxyURL, []domainegress.RouteRule{
		{Scope: domainegress.ScopeBuild, Class: domainegress.TrafficClassBilling, TargetMode: domainegress.RouteRuleTargetFixed, TargetNodeID: 404, Enabled: true},
	})
	// The repository has no node 404, so the rule target misses.

	request, err := http.NewRequestWithContext(
		infraegress.WithTrafficClass(context.Background(), domainegress.TrafficClassBilling),
		http.MethodGet, upstream.URL+"/billing", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("unavailable rule target must fall back, got error: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "upstream-body" {
		t.Fatalf("body = %q", body)
	}
	// No pool node is configured either, so the request went direct: the
	// invariant is the request survived the dead rule target.
	if got := atomic.LoadInt64(proxyCalls); got != 0 {
		t.Fatalf("proxy calls = %d, want 0 (rule target missing, pool empty)", got)
	}
}

func TestEgressTransportDirectRuleBypassesProxy(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	transport := newRouteRuleTransport(t, proxyURL, []domainegress.RouteRule{
		{Scope: domainegress.ScopeBuild, Class: domainegress.TrafficClassCredential, TargetMode: domainegress.RouteRuleTargetDirect, Enabled: true},
	})

	request, err := http.NewRequestWithContext(
		infraegress.WithTrafficClass(context.Background(), domainegress.TrafficClassCredential),
		http.MethodGet, upstream.URL+"/oauth2/token", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "upstream-body" {
		t.Fatalf("body = %q", body)
	}
	if got := atomic.LoadInt64(proxyCalls); got != 0 {
		t.Fatalf("proxy calls = %d, want 0 (direct rule must bypass the proxy)", got)
	}
}
