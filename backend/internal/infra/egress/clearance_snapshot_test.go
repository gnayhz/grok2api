package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func clearanceTestPtrTime(value time.Time) *time.Time { return &value }

// RefreshDueClearances 现经 1s 快照缓存读取节点。快照新鲜度语义必须与
// 直查等价:(1) 指纹不匹配的持久 clearance 状态触发求解(经缓存路径可见);
// (2) force=true 仍然刷新全部(忽略内存新鲜)。
func TestRefreshDueClearancesViaSnapshotCacheSemantics(t *testing.T) {
	var solves atomic.Int64
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		solves.Add(1)
		w.Header().Set("Content-Type", "application/json")
		body := `{"status":"ok","solution":{"url":"https://x.example/","userAgent":"ua","cf_clearance":"cf","ttl":300}}`
		w.Write([]byte(body))
	}))
	t.Cleanup(solver.Close)

	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &e2eRepo{pools: map[uint64]domain.Pool{}}
	encrypted, err := cipher.Encrypt("http://10.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	repo.nodes = append(repo.nodes, domain.Node{
		ID: 1, Name: "warp", Enabled: true, Health: 1, EncryptedProxyURL: encrypted,
		ClearanceRefreshedAt: clearanceTestPtrTime(time.Now().UTC().Add(-time.Minute)),
		ClearanceFingerprint: "", // 指纹不匹配 → 持久状态不算新鲜 → 应触发求解
	})
	manager := NewManager(repo, cipher)
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: solver.URL, TargetURL: "https://x.example/"})

	// (1) 指纹不匹配的持久状态必须触发求解(经快照缓存路径)。
	if err := manager.RefreshDueClearances(context.Background(), false); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if solves.Load() != 1 {
		t.Fatalf("fingerprint-mismatched persisted state did not solve: %d", solves.Load())
	}

	// (2) force=true 仍然再次求解(忽略内存新鲜)。
	before := solves.Load()
	if err := manager.RefreshDueClearances(context.Background(), true); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if solves.Load() != before+1 {
		t.Fatalf("force refresh did not re-solve: %d -> %d", before, solves.Load())
	}
}
