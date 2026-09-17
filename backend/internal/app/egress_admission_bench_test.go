package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// Both paths use the same real HTTP server, SQL routing repository and managed
// Go transport. The second adds the production authoritative quality adapter.
// This measures local admission cost, not external provider latency.
func TestRuntimeEndToEndAuthoritativeAdmissionCost(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "admission.db")
	db, err := relational.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(ctx, registry.Options{SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	repo := relational.NewEgressRepository(db)
	node, err := repo.CreateEgressNode(ctx, domain.Node{Name: "local-admission", Enabled: true, Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	plain, admitted := infraegress.NewManagerWithLimits(repo, nil, netbudget.Limits{}), infraegress.NewManagerWithLimits(repo, nil, netbudget.Limits{})
	defer plain.Close(ctx)
	defer admitted.Close(ctx)
	admitted.SetExitEligibility(qualityExitEligibility{registry: r})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "healthy payload") }))
	defer server.Close()
	call := func(m *infraegress.Manager) time.Duration {
		start := time.Now()
		lease, err := m.Acquire(ctx, domain.ScopeBuild, "benchmark")
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		if lease.NodeID != node.ID {
			t.Fatalf("measurement bypassed selected node: %d", lease.NodeID)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		res, err := lease.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		lease.Observe(res.StatusCode, nil)
		return time.Since(start)
	}
	for range 100 {
		call(plain)
		call(admitted)
	}
	percentile := func(samples []time.Duration, p int) time.Duration {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[(len(samples)-1)*p/100]
	}
	for batch := range 3 {
		base, withAdmission := make([]time.Duration, 500), make([]time.Duration, 500)
		for i := range base {
			base[i], withAdmission[i] = call(plain), call(admitted)
		}
		t.Logf("batch=%d managed p50=%v p95=%v p99=%v with SQLite authoritative admission p50=%v p95=%v p99=%v", batch, percentile(base, 50), percentile(base, 95), percentile(base, 99), percentile(withAdmission, 50), percentile(withAdmission, 95), percentile(withAdmission, 99))
	}
	// Closing the authoritative store must fail subsequent admission even with
	// a warm candidate snapshot and a reusable healthy connection.
	_ = r.Close()
	lease, err := admitted.Acquire(ctx, domain.ScopeBuild, "benchmark")
	if lease != nil {
		lease.Release()
	}
	if err == nil {
		t.Fatal("authoritative admission bypassed closed storage")
	}
}
