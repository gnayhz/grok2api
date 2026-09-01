package egress

import (
	"context"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// gatedSolver 可控求解器:门闩关闭时 Solve 立即失败(阻塞语义由调用方
// 的 select 超时承载),开启后返回新解。
type gatedSolver struct {
	gate chan struct{}
}

func newGatedSolver(open bool) *gatedSolver {
	s := &gatedSolver{gate: make(chan struct{})}
	if open {
		close(s.gate)
	}
	return s
}

func (s *gatedSolver) openGate() { close(s.gate) }

func (s *gatedSolver) Solve(ctx context.Context, _ ClearanceConfig, _ string) (clearanceSolution, error) {
	select {
	case <-s.gate:
		return clearanceSolution{Cookies: "cf_clearance=fresh", UserAgent: "UA-fresh"}, nil
	case <-ctx.Done():
		return clearanceSolution{}, ctx.Err()
	}
}

func primeRoutineExpiredState(t *testing.T, manager *Manager, key, proxyURL string, mutate func(*clearanceState)) {
	t.Helper()
	manager.clearanceMu.Lock()
	cfg := manager.clearanceConfig
	version := manager.clearanceVersion
	interval := clearanceRefreshInterval(cfg)
	manager.clearanceMu.Unlock()
	now := time.Now().UTC()
	state := clearanceState{
		cookies: "cf_clearance=stale", userAgent: "UA-stale",
		refreshedAt: now.Add(-(interval + 10*time.Second)),
		used:        true, version: version,
		fingerprint:        clearanceFingerprint(cfg, proxyURL),
		bindingFingerprint: clearanceBindingFingerprint(cfg, proxyURL),
		lastUsedAt:         now,
	}
	if mutate != nil {
		mutate(&state)
	}
	manager.clearanceMu.Lock()
	manager.clearances[key] = state
	manager.clearanceMu.Unlock()
}

func newClearanceTestManager(t *testing.T, solver *gatedSolver) *Manager {
	t.Helper()
	manager, _ := newPoolTestManager(t)
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr"})
	manager.clearanceMu.Lock()
	manager.solver = solver
	manager.clearanceMu.Unlock()
	return manager
}

// TestClearanceRoutineExpiryServesStaleWithBackgroundRefresh 锁定 serve-stale
// 契约:例行过期(超出刷新窗口但仍在宽限内)时请求立即拿到旧解,不同步
// 等待求解;后台刷新完成后缓存更新为新解。回归态:过期即同步等满求解
// 超时(默认 1m+30s 锁宽限),首个请求被完整阻塞。
func TestClearanceRoutineExpiryServesStaleWithBackgroundRefresh(t *testing.T) {
	solver := newGatedSolver(false)
	manager := newClearanceTestManager(t, solver)
	const proxyURL = "socks5://proxy:1080"
	key := clearanceCacheKey(5, proxyURL, false)
	node := domain.Node{ID: 5, Name: "n", Enabled: true, Health: 1}
	primeRoutineExpiredState(t, manager, key, proxyURL, nil)

	type result struct {
		cookies, ua string
		err         error
	}
	done := make(chan result, 1)
	go func() {
		cookies, ua, err := manager.ensureClearance(context.Background(), node, proxyURL, "", "", key, false)
		done <- result{cookies, ua, err}
	}()

	// 必须在短时间内返回旧解(求解器关闭;同步路径会阻塞到超时)。
	select {
	case got := <-done:
		if got.err != nil || got.cookies != "cf_clearance=stale" || got.ua != "UA-stale" {
			t.Fatalf("routine-expired ensureClearance = (%q, %q, %v), want stale solution immediately", got.cookies, got.ua, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("routine-expired ensureClearance blocked on synchronous solve; must serve stale")
	}

	// 后台刷新完成:缓存更新为新解。
	solver.openGate()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		manager.clearanceMu.Lock()
		state, ok := manager.clearances[key]
		manager.clearanceMu.Unlock()
		if ok && state.cookies == "cf_clearance=fresh" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background refresh did not update the cached clearance")
}

// TestClearanceInvalidStillForcesSynchronousRefresh 锁定 invalid(403 失效)
// 不走 serve-stale:旧解已「已知坏了」,必须同步强制刷新拿新解。
func TestClearanceInvalidStillForcesSynchronousRefresh(t *testing.T) {
	solver := newGatedSolver(true)
	manager := newClearanceTestManager(t, solver)
	const proxyURL = "socks5://proxy:1080"
	key := clearanceCacheKey(5, proxyURL, false)
	node := domain.Node{ID: 5, Name: "n", Enabled: true, Health: 1}
	primeRoutineExpiredState(t, manager, key, proxyURL, func(state *clearanceState) {
		state.invalid = true
	})

	cookies, ua, err := manager.ensureClearance(context.Background(), node, proxyURL, "", "", key, false)
	if err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if cookies != "cf_clearance=fresh" || ua != "UA-fresh" {
		t.Fatalf("invalid state must force synchronous fresh solve, got (%q, %q)", cookies, ua)
	}
}
