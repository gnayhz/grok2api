package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

// size 报告观察表当前大小(仅本测试断言使用);nil 接收者返回 0。
func (t *probeDeadTracker) size() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.obs)
}

func TestSecondReviewCapacityDoesNotCooldownUncontactedNode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits netbudget.Limits
	}{
		{"connections", netbudget.Limits{Connections: 1}},
		{"requests", netbudget.Limits{Requests: 1}},
		{"clients", netbudget.Limits{Clients: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := tc.limits
			limits.QueueTimeout = 20 * time.Millisecond
			assertProbeCapacityPreservesHealth(t, limits)
		})
	}
}

func assertProbeCapacityPreservesHealth(t *testing.T, limits netbudget.Limits) {
	t.Helper()
	ctx, service, repo := newPoolServiceFixture(t)
	m := infraegress.NewManagerWithLimits(repo, service.cipher, limits)
	defer m.Close(context.Background())
	defer service.Close(context.Background())
	service.SetNodeProber(m)
	service.SetQualityQuarantiner(m)
	var proxyCalls atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxyServer.Close()
	proxy := proxyServer.URL
	node, err := service.Create(ctx, Input{Name: "capacity-audit", Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(gate)
	tr, closeOwner, err := m.ManageHTTPTransport(ctx, &http.Transport{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOwner()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	before, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := repo.BeginEgressNodeProbe(ctx, node.ID, before.EncryptedProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateEgressNodeProbe(ctx, node.ID, before.EncryptedProxyURL, domain.ProbeResult{
		Revision: revision, Status: domain.ProbeStatusHealthy, TestedAt: time.Now(), ExitIP: "192.0.2.1",
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, TestedAt: time.Now(), ExitIP: "192.0.2.1"},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusUnknown},
	}); err != nil {
		t.Fatal(err)
	}
	before, err = repo.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := service.TestNode(ctx, node.ID)
		var executionErr *domain.ProbeExecutionError
		if !errors.As(err, &executionErr) || !errors.Is(err, netbudget.ErrCapacity) || result.Status != domain.ProbeStatusUnknown {
			t.Fatalf("capacity must be an incomplete probe: %+v, %v", result, err)
		}
		t.Logf("probe status=%s ipv4=%s ipv6=%s error=%s", result.Status, result.IPv4.Status, result.IPv6.Status, result.Error)
	}
	batch, err := service.TestNodes(ctx, []uint64{node.ID})
	var executionErr *domain.ProbeExecutionError
	if !errors.As(err, &executionErr) || batch.Unhealthy != 0 || batch.Healthy != 0 {
		t.Fatalf("batch classified local inability as node health: %+v %v", batch, err)
	}
	stored, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.HealthState(), stored.HealthState()) || stored.ProbeStatus != before.ProbeStatus || !reflect.DeepEqual(stored.IPv4Probe, before.IPv4Probe) || !reflect.DeepEqual(stored.IPv6Probe, before.IPv6Probe) {
		t.Fatalf("local capacity changed node health: before=%+v after=%+v", before, stored)
	}
	observations := service.probeDead.size()
	if observations != 0 || proxyCalls.Load() != 0 {
		t.Fatalf("false probe evidence: observations=%d proxyCalls=%d", observations, proxyCalls.Load())
	}
}
