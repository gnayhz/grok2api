package egress

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 轮换成功 → 立即驱动组合根注入的单节点观测回调(闭环尾延≤5m→秒级);
// 轮换失败(IP 未变)不得触发。
func TestRotationSuccessNotifiesObserver(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7", TestedAt: time.Now(),
		IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7"},
		IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::7"}}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7",
		IPv6Probe: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: "2001:db8::1"}}
	service, _, _, _ := newRotationTestService(t, node, true, probe)

	var mu sync.Mutex
	observed := []uint64{}
	service.SetRotationSuccessObserver(func(_ context.Context, nodeID uint64) {
		mu.Lock()
		observed = append(observed, nodeID)
		mu.Unlock()
	})
	service.processRotation(context.Background(), 3)
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 1 || observed[0] != 3 {
		t.Fatalf("observer calls = %v, want [3]", observed)
	}
}

func TestRotationFailureDoesNotNotifyObserver(t *testing.T) {
	probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "198.51.100.7", TestedAt: time.Now()}
	node := domain.Node{ID: 3, Name: "warp", Enabled: true, Health: 1, ExitIP: "198.51.100.7"}
	service, _, _, _ := newRotationTestService(t, node, true, probe)

	var called atomic.Int64
	service.SetRotationSuccessObserver(func(context.Context, uint64) { called.Add(1) })
	service.processRotation(context.Background(), 3)
	if called.Load() != 0 {
		t.Fatalf("failed rotation notified observer: %d", called.Load())
	}
}
