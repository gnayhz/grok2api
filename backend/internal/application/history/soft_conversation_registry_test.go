package history

import (
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// 同一会话从第 1 轮起键恒定:首轮写入的缓存在第 2 轮即被命中,
// 不存在第 1→2 轮的键轮换。
func TestSoftConversationNoRotationFromFirstTurn(t *testing.T) {
	resolver := &IdentityResolver{}
	resolver = &IdentityResolver{}
	turn1 := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello"}]}`)
	turn2 := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`)
	turn3 := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"},{"role":"assistant","content":"done"}]}`)
	first := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "req-1", turn1)
	second := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "req-2", turn2)
	third := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "req-3", turn3)
	if first.UpstreamID == "" || first.UpstreamID != second.UpstreamID || second.UpstreamID != third.UpstreamID {
		t.Fatalf("soft identity must stay constant from turn 1: %s %s %s", first.UpstreamID, second.UpstreamID, third.UpstreamID)
	}
}

// 同租户、相同开场白、不同回复的两条对话必须从第 2 轮起隔离。
func TestSoftConversationForksOnDivergentReplies(t *testing.T) {
	resolver := &IdentityResolver{}
	resolver = &IdentityResolver{}
	convA := []byte(`{"messages":[{"role":"user","content":"same opening"},{"role":"assistant","content":"answer A"},{"role":"user","content":"go on"}]}`)
	convB := []byte(`{"messages":[{"role":"user","content":"same opening"},{"role":"assistant","content":"answer B"},{"role":"user","content":"go on"}]}`)
	convA2 := []byte(`{"messages":[{"role":"user","content":"same opening"},{"role":"assistant","content":"answer A"},{"role":"user","content":"go on"},{"role":"assistant","content":"more A"}]}`)
	a1 := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "req-1", convA)
	b1 := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "req-2", convB)
	a2 := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "req-3", convA2)
	if a1.UpstreamID == b1.UpstreamID {
		t.Fatal("divergent conversations shared the upstream session")
	}
	if a1.UpstreamID != a2.UpstreamID {
		t.Fatal("forked conversation must keep its own id on later turns")
	}
}

// 不同租户(不同 client key)即使请求逐字节相同也必须隔离。
func TestSoftConversationTenantIsolation(t *testing.T) {
	resolver := &IdentityResolver{}
	resolver = &IdentityResolver{}
	body := []byte(`{"input":[{"role":"user","content":"identical question"}]}`)
	tenant1 := resolver.resolveTestIdentity(1, accountdomain.ProviderBuild, "grok-4.6", "", "", "r1", body)
	tenant2 := resolver.resolveTestIdentity(2, accountdomain.ProviderBuild, "grok-4.6", "", "", "r2", body)
	if tenant1.UpstreamID == tenant2.UpstreamID {
		t.Fatal("different client keys shared the upstream session")
	}
}

// 键为确定性派生:登记表清空(模拟进程重启)后同一对话原样重建,
// 分叉对话在下一次请求时被重新识别。
func TestSoftConversationDeterministicAcrossRegistryLoss(t *testing.T) {
	resolver := &IdentityResolver{}
	resolver = &IdentityResolver{}
	turn2 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`)
	before := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "req-1", turn2)
	resolver = &IdentityResolver{}
	after := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "req-2", turn2)
	if before.UpstreamID != after.UpstreamID {
		t.Fatal("soft identity must be deterministic across registry loss")
	}
}

// 登记表按空闲回收:被回收的对话凭确定性键继续工作(仅分叉检测退化)。
func TestSoftConversationRegistrySweep(t *testing.T) {
	reg := newSoftConversationRegistry()
	id := reg.resolveSoftConversation("open-1", "open-1", "base-1", "fork-1")
	if id != "base-1" {
		t.Fatalf("first resolve should use the base id, got %s", id)
	}
	id = reg.resolveSoftConversation("open-1", "full-1", "base-1", "fork-1")
	if id != "base-1" {
		t.Fatalf("first reply must bind to the base conversation, got %s", id)
	}
	id = reg.resolveSoftConversation("open-1", "full-2", "base-1", "fork-2")
	if id != "fork-2" {
		t.Fatalf("divergent reply must fork, got %s", id)
	}
	// 强制过期回收后,基线对话仍按确定性键解析。
	reg.mu.Lock()
	for _, entry := range reg.entries {
		entry.lastSeen = time.Now().Add(-2 * softConversationIdleTTL)
	}
	reg.lastSweep = time.Now().Add(-2 * softConversationSweepInterval)
	reg.sweepLocked(time.Now())
	remain := len(reg.entries)
	reg.mu.Unlock()
	if remain != 0 {
		t.Fatalf("idle entries should be swept, %d remain", remain)
	}
	id = reg.resolveSoftConversation("open-1", "full-1", "base-1", "fork-1")
	if id != "base-1" {
		t.Fatalf("base conversation survives registry loss deterministically, got %s", id)
	}
}
