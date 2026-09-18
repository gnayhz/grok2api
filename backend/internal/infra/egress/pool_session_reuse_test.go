package egress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

func TestSessionReusePoolDistributesNewSessionsAndRetainsAccountSwitches(t *testing.T) {
	m, repo := newPoolTestManager(t)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategySessionReuse}
	repo.member[1] = sessionTestNodes(1, 2, 3, 4, 5)
	assigned := make(map[uint64]int)
	for i := range 25 {
		ctx := WithBuildSession(context.Background(), fmt.Sprintf("synthetic-session-%d", i))
		var first uint64
		for turn := range 3 {
			lease, outcome, err := m.AcquirePoolRouted(ctx, domain.ScopeBuild, fmt.Sprintf("account-%d", turn), 1, false, "")
			if err != nil || lease == nil || outcome != PoolRouteMember {
				t.Fatalf("acquire: %v %v", outcome, err)
			}
			if turn == 0 {
				first = lease.NodeID
				assigned[first]++
			} else if lease.NodeID != first {
				t.Errorf("account switch moved session %d: %d -> %d", i, first, lease.NodeID)
			}
			lease.Release()
		}
	}
	for _, node := range repo.member[1] {
		if assigned[node.ID] != 5 {
			t.Fatalf("new sessions concentrated on account affinity: %v", assigned)
		}
	}
}

func TestSessionReusePoolReselectsWithinQualifiedMembers(t *testing.T) {
	m, repo := newPoolTestManager(t)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategySessionReuse}
	repo.member[1] = sessionTestNodes(1, 2, 3)
	ctx := WithBuildSession(context.Background(), "synthetic-stable")
	acquire := func(ctx context.Context) uint64 {
		t.Helper()
		lease, _, err := m.AcquirePoolRouted(ctx, domain.ScopeBuild, "account", 1, false, "")
		if err != nil || lease == nil {
			t.Fatalf("acquire: %v", err)
		}
		defer lease.Release()
		return lease.NodeID
	}
	first := acquire(ctx)
	m.SetExitEligibility(stubExitEligibility{ineligible: map[uint64]bool{first: true}})
	second := acquire(ctx)
	if second == first {
		t.Fatal("quality-ineligible pin reused")
	}
	m.SetExitEligibility(stubExitEligibility{})
	if got := acquire(ctx); got != second {
		t.Fatalf("recovery stole session back: %d -> %d", second, got)
	}
	third := acquire(physical.WithNodeExclusions(ctx, map[uint64]struct{}{first: {}, second: {}}))
	if third == first || third == second {
		t.Fatal("pin overrode retry exclusions")
	}
	repo.member[1] = sessionTestNodes(first)
	m.InvalidatePoolCache()
	if got := acquire(ctx); got != first {
		t.Fatal("removed member retained its pin")
	}
	repo.member[1] = nil
	m.InvalidatePoolCache()
	lease, outcome, err := m.AcquirePoolRouted(ctx, domain.ScopeBuild, "account", 1, false, "")
	if err != nil || lease != nil || outcome != PoolRouteNone {
		t.Fatalf("empty pool escaped its boundary: %v %v %v", lease, outcome, err)
	}
}

func TestSessionReuseAllocationConcurrencyLoadExpiryAndPoolScope(t *testing.T) {
	var state sessionReuseRoutes
	nodes := sessionTestNodes(1, 2, 3, 4, 5)
	zero := func(uint64) int64 { return 0 }
	now := time.Now()
	var wg sync.WaitGroup
	results := make(chan uint64, 80)
	for range 80 {
		wg.Go(func() { results <- state.selectNode(1, "same", nodes, now, zero).ID })
	}
	wg.Wait()
	close(results)
	first := <-results
	for id := range results {
		if id != first {
			t.Fatal("concurrent first requests created different bindings")
		}
	}
	if len(state.pins) != 1 || state.assigned[first] != 1 {
		t.Fatal("concurrent requests counted as independent sessions")
	}
	var distributed sessionReuseRoutes
	for i := range 100 {
		wg.Go(func() { distributed.selectNode(1, fmt.Sprint(i), nodes, now, zero) })
	}
	wg.Wait()
	for _, node := range nodes {
		if distributed.assigned[node.ID] != 20 {
			t.Fatalf("concurrent new sessions concentrated: %v", distributed.assigned)
		}
	}
	busy := func(id uint64) int64 {
		if id == first {
			return 0
		}
		return 3
	}
	if got := state.selectNode(1, "new", nodes, now, busy).ID; got != first {
		t.Fatal("new session ignored in-flight load")
	}
	otherPool := state.selectNode(2, "same", nodes, now, zero).ID
	if otherPool == first {
		t.Fatal("pool inherited another pool's session binding")
	}
	// Expiry must be checked before refreshing an existing pin.
	after := now.Add(sessionPinIdleTTL)
	if got := state.selectNode(1, "same", nodes, after, func(id uint64) int64 {
		if id == otherPool {
			return 0
		}
		return 1
	}).ID; got != otherPool {
		t.Fatal("expired session was refreshed without reallocation")
	}
	if len(state.pins) != 1 || len(state.assigned) != 1 {
		t.Fatal("expired assignments still influence load")
	}
	for i := range maxSessionPinnedNodes + 50 {
		state.selectNode(1, fmt.Sprint(i), nodes, after, zero)
	}
	if len(state.pins) != maxSessionPinnedNodes || state.recent.Len() != maxSessionPinnedNodes {
		t.Fatal("session retention is unbounded")
	}
	sum := 0
	for _, count := range state.assigned {
		sum += count
	}
	if sum != len(state.pins) {
		t.Fatal("evicted pins retained their load")
	}
}

func TestSessionReusePoolWithoutBuildIdentityUsesRandomSelection(t *testing.T) {
	for _, scope := range []domain.Scope{domain.ScopeBuild, domain.ScopeWeb, domain.ScopeConsole} {
		t.Run(string(scope), func(t *testing.T) {
			m, repo := newPoolTestManager(t)
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategySessionReuse}
			repo.member[1] = sessionTestNodes(1, 2, 3)
			ctx := context.Background()
			if scope != domain.ScopeBuild {
				ctx = WithBuildSession(ctx, "synthetic-wrong-scope")
			}
			seen := map[uint64]bool{}
			for range 100 {
				lease, _, err := m.AcquirePoolRouted(ctx, scope, "same-account", 1, false, "")
				if err != nil || lease == nil {
					t.Fatalf("acquire: %v", err)
				}
				seen[lease.NodeID] = true
				lease.Release()
			}
			if len(seen) != 3 || len(m.routing.sessionReuse.pins) != 0 {
				t.Fatalf("unsupported session created affinity: seen=%v pins=%d", seen, len(m.routing.sessionReuse.pins))
			}
		})
	}
}

func TestSessionReusePoolUsesRealConnectionsAcrossAccountChanges(t *testing.T) {
	m, repo := newPoolTestManager(t)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategySessionReuse}
	var connections atomic.Int64
	var requests atomic.Int64
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer synthetic-A" && got != "Bearer synthetic-B" {
			t.Errorf("missing per-request credential: %q", got)
		}
		requests.Add(1)
		_, _ = io.WriteString(w, "synthetic response")
	}))
	proxy.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	proxy.Start()
	t.Cleanup(proxy.Close)
	node := sessionTestNodes(1)[0]
	node.EncryptedProxyURL = encryptedProxy(t, m.cipher, proxy.URL)
	repo.member[1] = []domain.Node{node}
	ctx := WithBuildSession(context.Background(), "synthetic-socket-session")
	for _, account := range []string{"synthetic-A", "synthetic-B"} {
		lease, _, err := m.AcquirePoolRouted(WithAccountIdentity(ctx, account), domain.ScopeBuild, account, 1, false, "")
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.invalid/synthetic", nil)
		req.Header.Set("Authorization", "Bearer "+account)
		response, err := lease.Do(req)
		if err != nil {
			lease.Release()
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		lease.Release()
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 || connections.Load() != 1 {
		t.Fatalf("requests=%d connections=%d; account switch lost connection reuse", requests.Load(), connections.Load())
	}
}
