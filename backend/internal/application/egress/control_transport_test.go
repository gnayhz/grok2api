package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

type countedHTTPOwner struct {
	*infraegress.Manager
	calls atomic.Int32
}

func (o *countedHTTPOwner) ManageHTTPTransport(ctx context.Context, tr *http.Transport) (http.RoundTripper, func(), error) {
	o.calls.Add(1)
	return o.Manager.ManageHTTPTransport(ctx, tr)
}

func TestControlHTTPUsesRuntimeAndPreservesSubscriptionDestinationGuard(t *testing.T) {
	m := infraegress.NewManager(nil, nil)
	defer m.Close(context.Background())
	owner := &countedHTTPOwner{Manager: m}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if s := m.RuntimeStats(); s.Network.Requests != 1 || s.Network.Connections != 1 || s.Network.Clients != 1 {
			t.Errorf("webhook bypassed runtime: %+v", s)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	s := NewService(nil, nil)
	s.SetHTTPTransportOwner(owner)
	if err := s.callRotationWebhook(context.Background(), server.URL, RotationConfig{WebhookTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchProxySubscription(context.Background(), server.URL, "", owner); err == nil {
		t.Fatal("managed subscription bypassed private-address guard")
	}
	if owner.calls.Load() != 2 || hits.Load() != 1 {
		t.Fatalf("control transport ownership=%d actual requests=%d", owner.calls.Load(), hits.Load())
	}
	if s := m.RuntimeStats(); s.Network.Clients != 0 || s.Network.Requests != 0 || s.Network.Connections != 0 {
		t.Fatalf("control request resources remained: %+v", s)
	}
}
