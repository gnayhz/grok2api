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

func newRouteRuleTransport(t *testing.T, proxyURL string, config domainegress.OperationsConfig) *egressTransport {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	node := domainegress.Node{ID: 21, Name: "rule-exit", Enabled: true, EncryptedProxyURL: encryptedProxy}
	repo := routeRuleEgressRepository{nodes: map[uint64]domainegress.Node{21: node}, config: config}
	manager := infraegress.NewManager(repo, cipher)
	return &egressTransport{manager: manager, fallback: http.DefaultTransport}
}

// newRouteRuleTwoExitTransport builds a transport with a rule exit (node 21)
// and a separate exit (node 31) so assertions can tell which path a request
// actually took.
func newRouteRuleTwoExitTransport(t *testing.T, ruleProxyURL, otherProxyURL string, config domainegress.OperationsConfig) *egressTransport {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	ruleProxy, err := cipher.Encrypt(ruleProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	otherProxy, err := cipher.Encrypt(otherProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[uint64]domainegress.Node{
		21: {ID: 21, Name: "rule-exit", Enabled: true, EncryptedProxyURL: ruleProxy},
		31: {ID: 31, Name: "other-exit", Enabled: true, EncryptedProxyURL: otherProxy},
	}
	repo := routeRuleEgressRepository{nodes: nodes, config: config}
	manager := infraegress.NewManager(repo, cipher)
	return &egressTransport{manager: manager, fallback: http.DefaultTransport}
}

func classNodeTarget(class domainegress.TrafficClass, nodeID uint64) domainegress.OperationsConfig {
	return domainegress.OperationsConfig{ClassTargets: map[domainegress.TrafficClass]domainegress.RoutingTarget{
		class: {Mode: domainegress.RoutingTargetNode, NodeID: nodeID},
	}}
}

func TestEgressTransportRoutesByTrafficClassTarget(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	transport := newRouteRuleTransport(t, proxyURL, classNodeTarget(domainegress.TrafficClassBilling, 21))

	// Billing class honors the class target and exits through the fixed node.
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
		t.Fatalf("proxy calls = %d, want 1 (class target must pin the exit)", got)
	}

	// Credential class has no target and no pool nodes: the direct fallback
	// keeps the counter unchanged.
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

// Class targets outrank scope targets: a billing class pinned to node 21 must
// not be rerouted by the build scope target pointing at node 31.
func TestEgressTransportClassTargetOutranksScopeTarget(t *testing.T) {
	ruleProxyURL, ruleCalls, upstream := newRouteRuleProxyPair(t)
	otherProxyURL, otherCalls, _ := newRouteRuleProxyPair(t)
	transport := newRouteRuleTwoExitTransport(t, ruleProxyURL, otherProxyURL, domainegress.OperationsConfig{
		ScopeTargets: map[domainegress.Scope]domainegress.RoutingTarget{
			domainegress.ScopeBuild: {Mode: domainegress.RoutingTargetNode, NodeID: 31},
		},
		ClassTargets: map[domainegress.TrafficClass]domainegress.RoutingTarget{
			domainegress.TrafficClassBilling: {Mode: domainegress.RoutingTargetNode, NodeID: 21},
		},
	})

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
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if got := atomic.LoadInt64(ruleCalls); got != 1 {
		t.Fatalf("class exit calls = %d, want 1 (class target wins)", got)
	}
	if got := atomic.LoadInt64(otherCalls); got != 0 {
		t.Fatalf("scope exit calls = %d, want 0", got)
	}
}

// Without an explicit traffic class the request defaults to inference, so the
// inference class target still routes it.
func TestEgressTransportDefaultClassIsInference(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	transport := newRouteRuleTransport(t, proxyURL, classNodeTarget(domainegress.TrafficClassInference, 21))

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
		t.Fatalf("proxy calls = %d, want 1 (default inference class follows the target)", got)
	}
}

// A pinned node (quality canary) bypasses routing entirely.
func TestEgressTransportPinnedNodeBypassesRouting(t *testing.T) {
	ruleProxyURL, ruleCalls, upstream := newRouteRuleProxyPair(t)
	otherProxyURL, otherCalls, _ := newRouteRuleProxyPair(t)
	transport := newRouteRuleTwoExitTransport(t, ruleProxyURL, otherProxyURL, classNodeTarget(domainegress.TrafficClassInference, 21))

	pinned := infraegress.WithPinnedNode(context.Background(), 31)
	request, err := http.NewRequestWithContext(pinned, http.MethodGet, upstream.URL+"/v1/responses", nil)
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
		t.Fatalf("rule exit calls = %d, want 0 (pinned node must not be rerouted)", got)
	}
	if got := atomic.LoadInt64(otherCalls); got != 1 {
		t.Fatalf("pinned exit calls = %d, want 1", got)
	}
}

// A node target that became unavailable must not fail the request: the
// transport falls back to the automatic schedule and records the outcome.
func TestEgressTransportFallsBackWhenNodeTargetUnavailable(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	transport := newRouteRuleTransport(t, proxyURL, classNodeTarget(domainegress.TrafficClassBilling, 404))
	// The repository has no node 404, so the target misses.

	request, err := http.NewRequestWithContext(
		infraegress.WithTrafficClass(context.Background(), domainegress.TrafficClassBilling),
		http.MethodGet, upstream.URL+"/billing", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("unavailable node target must fall back, got error: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "upstream-body" {
		t.Fatalf("body = %q", body)
	}
	// No automatic node is configured either, so the request went direct: the
	// invariant is the request survived the dead target.
	if got := atomic.LoadInt64(proxyCalls); got != 0 {
		t.Fatalf("proxy calls = %d, want 0 (target missing, pool empty)", got)
	}
}

func TestEgressTransportDirectTargetBypassesProxy(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	transport := newRouteRuleTransport(t, proxyURL, domainegress.OperationsConfig{
		ClassTargets: map[domainegress.TrafficClass]domainegress.RoutingTarget{
			domainegress.TrafficClassCredential: {Mode: domainegress.RoutingTargetDirect},
		},
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
		t.Fatalf("proxy calls = %d, want 0 (direct target must bypass the proxy)", got)
	}
}
