package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

func TestDefaultBuildFallbackUsesRuntimeBudgetAndShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "healthy") }))
	defer server.Close()
	manager := infraegress.NewManagerWithLimits(&routeRuleEgressRepository{}, nil, netbudget.Limits{Requests: 1, Connections: 1, Clients: 2})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	transport := &egressTransport{manager: manager, fallback: newBuildDirectTransport(time.Second)}
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	res, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if s := manager.RuntimeStats(); s.Network.Requests != 1 || s.Network.Connections != 1 || s.Network.Clients != 1 {
		t.Fatalf("unconfigured Build bypassed runtime: %+v", s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = transport.RoundTrip(req.Clone(ctx)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("default Build did not wait for its request budget: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if err = manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := manager.RuntimeStats(); s.Network.Requests != 0 || s.Network.Connections != 0 || s.Network.Clients != 0 {
		t.Fatalf("default Build resources survived shutdown: %+v", s)
	}
	if _, err = transport.RoundTrip(req); !errors.Is(err, infraegress.ErrRuntimeClosed) {
		t.Fatalf("default Build accepted work after shutdown: %v", err)
	}
}

type blockedFeedbackRepository struct {
	routeRuleEgressRepository
	reads   atomic.Int32
	entered chan context.Context
	gate    chan struct{}
}

func (r *blockedFeedbackRepository) ApplyEgressHealthObservation(ctx context.Context, o domain.HealthObservation) (domain.HealthState, error) {
	r.entered <- ctx
	select {
	case <-r.gate:
	case <-ctx.Done():
		return domain.HealthState{}, ctx.Err()
	}
	n, err := r.routeRuleEgressRepository.GetEgressNode(ctx, o.NodeID)
	if err != nil {
		return domain.HealthState{}, err
	}
	state, _ := n.HealthState().Apply(o)
	return state, nil
}

func TestRuntimeHealthyResponseNotBlockedByFeedbackStorage(t *testing.T) {
	proxyURL, proxyCalls, upstream := newRouteRuleProxyPair(t)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxy, _ := cipher.Encrypt(proxyURL)
	repo := &blockedFeedbackRepository{
		routeRuleEgressRepository: routeRuleEgressRepository{
			nodes:  map[uint64]domain.Node{21: {ID: 21, Name: "healthy-proxy", Enabled: true, Health: 0.7, FailureCount: 1, EncryptedProxyURL: proxy}},
			config: classNodeTarget(domain.TrafficClassInference, 21),
		},
		entered: make(chan context.Context, 1), gate: make(chan struct{}),
	}
	manager := infraegress.NewManagerWithLimits(repo, cipher, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	transport := &egressTransport{manager: manager, fallback: http.DefaultTransport}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = infraegress.WithTrafficClass(ctx, domain.TrafficClassInference)
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
		response, err := transport.RoundTrip(req)
		if response != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		close(repo.gate)
		if err != nil {
			t.Fatalf("healthy proxy request: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		var feedbackCtx context.Context
		select {
		case feedbackCtx = <-repo.entered:
		default:
		}
		if feedbackCtx != nil {
			_, deadline := feedbackCtx.Deadline()
			t.Errorf("healthy proxy responded (calls=%d), but delivery waits for feedback storage; deadline=%v", atomic.LoadInt64(proxyCalls), deadline)
		} else {
			t.Error("healthy proxy response was not delivered within the contract budget")
		}
		cancel()
		close(repo.gate)
		<-done
	}
}
