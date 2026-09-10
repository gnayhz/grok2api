package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

func TestControlTransportRetainsActiveBodyAndCustomDialPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "healthy") }))
	defer server.Close()
	m := NewManager(egressRepositoryTestStub{}, nil)
	defer m.Close(context.Background())
	var dials atomic.Int32
	tr := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	managed, closeOwner, err := m.ManageHTTPTransport(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOwner()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	res, err := managed.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	closeOwner()
	if s := m.RuntimeStats(); s.Network.Requests != 1 || s.Network.Clients != 1 || s.Network.Connections != 1 || s.Network.Dialing != 0 || s.RetiredClients != 1 || dials.Load() != 1 {
		t.Fatalf("retired control client lost ownership or dial policy: %+v, dials=%d", s, dials.Load())
	}
	if _, err = io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	// net/http may still be finishing its read loop when EOF reaches the
	// consumer. Socket ownership must settle after that close, not vanish early.
	deadline := time.Now().Add(time.Second)
	for m.RuntimeStats().Network.Connections != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := m.RuntimeStats(); s.Network.Requests != 0 || s.Network.Clients != 0 || s.Network.Connections != 0 {
		t.Fatalf("completed control client retained resources: %+v", s)
	}
}

func TestClearanceRPCSharesPhysicalRequestBudget(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","solution":{"userAgent":"test-agent","cookies":[{"name":"cf_clearance","value":"cookie"}]}}`)
	}))
	defer server.Close()
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{Requests: 1})
	defer m.Close(context.Background())
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := m.clearance.solver.Solve(context.Background(), ClearanceConfig{FlareSolverrURL: server.URL, Timeout: time.Second}, "")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("solver RPC did not start")
	}
	if s := m.RuntimeStats(); s.Network.Requests != 1 || s.Network.Clients != 1 || s.Network.Connections != 1 {
		t.Fatalf("solver bypassed network ownership: %+v", s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	lease, err := m.AcquireBuildEnvironmentDirect(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if _, err = lease.Do(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("solver and provider did not share request budget: %v", err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := m.RuntimeStats(); s.Network.Requests != 0 || s.Network.Connections != 0 {
		t.Fatalf("solver retained requests or sockets: %+v", s)
	}
}
