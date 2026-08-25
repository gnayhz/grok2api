package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// peerHeldLock 模拟分布式锁被另一实例持有:Acquire 永不成功。
type peerHeldLock struct{}

func (peerHeldLock) Acquire(context.Context, string, time.Duration) (func(), bool, error) {
	return nil, false, nil
}

// generationSwitchRepo 在首次读取(RefreshClearance 入口,即 refreshAfter
// 的世代来源)之后切换到对端持久化的新世代,模拟"等待期间对端完成了新
// 求解并落库"。switchAfter<=0 表示永不切换(对端始终未交付)。
type generationSwitchRepo struct {
	*e2eRepo
	switchAfter int64
	reads       atomic.Int64
	newCookie   string
}

func (r *generationSwitchRepo) GetEgressNode(ctx context.Context, id uint64) (domain.Node, error) {
	node, err := r.e2eRepo.GetEgressNode(ctx, id)
	if err != nil {
		return domain.Node{}, err
	}
	if r.switchAfter > 0 && r.reads.Add(1) > r.switchAfter {
		node.ClearanceRefreshedAt = clearanceTestPtrTime(time.Now().UTC())
		node.EncryptedCloudflareCookie = r.newCookie
		node.UserAgent = "ua-peer-new"
	}
	return node, nil
}

// newAdminForceFixture 构造带旧世代持久 clearance 的节点、指定分布式锁
// 行为与计数 solver。返回 (manager, solves)。
func newAdminForceFixture(t *testing.T, switchAfter int64, lock repository.DistributedLock) (*Manager, *atomic.Int64) {
	t.Helper()
	solves := &atomic.Int64{}
	solverBody := fmt.Sprintf("{%q:%q,%q:{%q:%q,%q:%q,%q:%q,%q:%d}}",
		"status", "ok", "solution", "url", "https://x.example/", "userAgent", "ua",
		"cf_clearance", "solver", "ttl", 300)
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		solves.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(solverBody))
	}))
	t.Cleanup(solver.Close)

	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyPlain := "http://10.0.0.1:8080"
	encryptedProxy, err := cipher.Encrypt(proxyPlain)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ClearanceConfig{
		Mode: "flaresolverr", FlareSolverrURL: solver.URL, TargetURL: "https://x.example/",
		Timeout: 700 * time.Millisecond,
	}
	oldRefreshed := time.Now().UTC().Add(-time.Minute)
	staleCookie, err := cipher.Encrypt("cf_clearance=stale-generation")
	if err != nil {
		t.Fatal(err)
	}
	freshCookie, err := cipher.Encrypt("cf_clearance=peer-fresh-generation")
	if err != nil {
		t.Fatal(err)
	}
	repo := &generationSwitchRepo{e2eRepo: &e2eRepo{}, switchAfter: switchAfter, newCookie: freshCookie}
	repo.egressRepositoryTestStub.nodes = append(repo.egressRepositoryTestStub.nodes, domain.Node{
		ID: 1, Name: "warp", Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy,
		ClearanceRefreshedAt:        &oldRefreshed,
		ClearanceFingerprint:        clearanceFingerprint(cfg, proxyPlain),
		ClearanceBindingFingerprint: clearanceBindingFingerprint(cfg, proxyPlain),
		EncryptedCloudflareCookie:   staleCookie, UserAgent: "ua-old",
	})
	manager := NewManager(repo, cipher)
	manager.SetClearanceLock(lock)
	manager.UpdateClearanceConfig(cfg)
	return manager, solves
}

// 回归锁(P2,管理员语义被静默降级):管理员强制刷新 Clearance 时若分布式
// 锁被对端持有,等待路径不得把管理员明确要求替换的旧世代当作"刷新成功"
// 返回——旧 cookie 会被重新缓存为有效,下一个请求继续 403。期望:只接受
// 严格更新的世代;对端超时未交付新世代则如实返回"另一个实例正在刷新"。
func TestAdminForceRefreshWithPeerLockRejectsStaleGeneration(t *testing.T) {
	manager, solves := newAdminForceFixture(t, 0, peerHeldLock{}) // 对端始终未交付新世代

	err := manager.RefreshClearance(context.Background(), 1)

	if err == nil {
		t.Fatal("force refresh accepted the stale persisted generation as success while the peer held the lock")
	}
	if !strings.Contains(err.Error(), "另一个实例正在刷新") {
		t.Fatalf("expected peer-holds-lock error, got %v", err)
	}
	if solves.Load() != 0 {
		t.Fatalf("solver must never run while the lock is held by the peer: %d solves", solves.Load())
	}
}

// 正向护栏:对端在等待期间持久化了严格更新的世代时,管理员强制刷新必须
// 复用对端的新结果并成功返回(而不是误报锁冲突,也不重复求解)。
func TestAdminForceRefreshWaitsForPeerNewGeneration(t *testing.T) {
	manager, solves := newAdminForceFixture(t, 1, peerHeldLock{}) // 首读(入口世代)后切换到新世代

	err := manager.RefreshClearance(context.Background(), 1)

	if err != nil {
		t.Fatalf("peer-delivered newer generation must be reused: %v", err)
	}
	if solves.Load() != 0 {
		t.Fatalf("peer's newer generation must be reused without a local solve: %d solves", solves.Load())
	}
	manager.clearanceMu.Lock()
	state, ok := manager.clearances[clearanceCacheKey(1, "http://10.0.0.1:8080", false)]
	manager.clearanceMu.Unlock()
	if !ok || !strings.Contains(state.cookies, "peer-fresh-generation") {
		t.Fatalf("cached clearance must be the peer's fresh generation: ok=%v cookies=%q", ok, state.cookies)
	}
}

// 锁获取路径的 force 语义护栏:管理员强制刷新拿到锁时,同世代(入口读取
// 时的同一持久世代)不得被复用——必须真正求解出新 cookie。
func TestAdminForceRefreshWithLockReSolvesSameGeneration(t *testing.T) {
	manager, solves := newAdminForceFixture(t, 0, alwaysAcquiredDistributedLock{}) // 永远同世代

	err := manager.RefreshClearance(context.Background(), 1)

	if err != nil {
		t.Fatalf("force refresh with lock: %v", err)
	}
	if solves.Load() != 1 {
		t.Fatalf("same-generation persisted state must be re-solved: solves=%d", solves.Load())
	}
}

// 锁获取路径的复用护栏:入口读取与拿到锁之间,对端持久化了严格更新的
// 世代(manager.go 2117 注释所述窗口)——复用它,不重复求解。
func TestAdminForceRefreshWithLockReusesNewerInterimGeneration(t *testing.T) {
	manager, solves := newAdminForceFixture(t, 1, alwaysAcquiredDistributedLock{}) // 首读旧世代,后续读新世代

	err := manager.RefreshClearance(context.Background(), 1)

	if err != nil {
		t.Fatalf("force refresh with lock: %v", err)
	}
	if solves.Load() != 0 {
		t.Fatalf("newer interim generation must be reused without solving: solves=%d", solves.Load())
	}
	manager.clearanceMu.Lock()
	state, ok := manager.clearances[clearanceCacheKey(1, "http://10.0.0.1:8080", false)]
	manager.clearanceMu.Unlock()
	if !ok || !strings.Contains(state.cookies, "peer-fresh-generation") {
		t.Fatalf("cached clearance must be the interim fresh generation: ok=%v cookies=%q", ok, state.cookies)
	}
}
