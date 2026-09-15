package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// TestExitIdentityDecisionFor 用人工构造样本固定双族比较规则:
// 任一非空族不同 → 翻篇;空族不参与比较;旧档案缺失的 IPv6 首次出现
// → 采纳补写而不翻篇(升级不得释放仍有效的限制)。
func TestExitIdentityDecisionFor(t *testing.T) {
	legacyV4Row := qIPEpochModel{CurrentIP: "192.0.2.1"}
	legacyV6Row := qIPEpochModel{CurrentIP: "2001:db8::1"}
	dualRow := qIPEpochModel{CurrentIP: "192.0.2.1", CurrentIPv6: "2001:db8::1"}
	cases := []struct {
		name     string
		stored   qIPEpochModel
		observed model.ExitIdentity
		advance  bool
		adoptV6  bool
	}{
		{"same dual identity", dualRow, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::1"}, false, false},
		{"ipv4 changed", dualRow, model.ExitIdentity{IPv4: "192.0.2.9", IPv6: "2001:db8::1"}, true, false},
		{"ipv6 only changed", dualRow, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::2"}, true, false},
		{"legacy row adopts ipv6", legacyV4Row, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::5"}, false, true},
		{"legacy row still advances on v4 change", legacyV4Row, model.ExitIdentity{IPv4: "192.0.2.9", IPv6: "2001:db8::5"}, true, false},
		{"legacy v6 aggregate classified as v6", legacyV6Row, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::1"}, false, false},
		{"legacy v6 aggregate detects v6 change", legacyV6Row, model.ExitIdentity{IPv6: "2001:db8::2"}, true, false},
		{"observed missing family is not a change", dualRow, model.ExitIdentity{IPv4: "192.0.2.1"}, false, false},
		{"observed empty v6 after adoption is not a change", dualRow, model.ExitIdentity{IPv4: "192.0.2.1"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := exitIdentityDecisionFor(c.stored, c.observed)
			if got.Advance != c.advance || got.AdoptV6 != c.adoptV6 {
				t.Fatalf("decision = %+v, want advance=%v adoptV6=%v", got, c.advance, c.adoptV6)
			}
		})
	}
}

func openIdentityRegistry(t *testing.T) *Registry {
	t.Helper()
	ctx := context.Background()
	r, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "identity.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func banCurrentExit(t *testing.T, r *Registry, nodeID uint64) {
	t.Helper()
	ctx := context.Background()
	epoch := r.CurrentEpoch(nodeID)
	id, err := r.OpenInvestigation(ctx, 9, model.EpochKey{NodeID: nodeID, Epoch: epoch}, time.Now(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictExitGuilty, `{}`, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if allowed, err := r.ExitAllowed(ctx, nodeID); err != nil || allowed {
		t.Fatalf("ban not in force: %v %v", allowed, err)
	}
}

// TestObserveExitIdentityIPv6RotationReleasesBan 固定核心修复行为:
// IPv4 稳定、仅 IPv6 轮换(WARP/MicroWARP 重启常态)→ epoch 翻篇并按
// 统一 ban 律释放旧限制。旧实现只比较聚合 IP(IPv4 优先),这类真实
// 换 IP 永远不会解禁,节点陷入"轮换成功↔ban 不解除"的循环。
func TestObserveExitIdentityIPv6RotationReleasesBan(t *testing.T) {
	r := openIdentityRegistry(t)
	ctx := context.Background()
	if _, _, _, err := r.ObserveExitIdentity(ctx, 5, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::1"}, 1); err != nil {
		t.Fatal(err)
	}
	banCurrentExit(t, r, 5)
	old, current, released, err := r.ObserveExitIdentity(ctx, 5, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::2"}, 2)
	if err != nil || old != 0 || current != 1 {
		t.Fatalf("ipv6 rotation must advance epoch: old=%d current=%d err=%v", old, current, err)
	}
	if len(released) != 1 {
		t.Fatalf("ipv6 rotation must release the old ban: %+v", released)
	}
	if allowed, err := r.ExitAllowed(ctx, 5); err != nil || !allowed {
		t.Fatalf("exit still blocked after ipv6 rotation: %v %v", allowed, err)
	}
}

// TestObserveExitIdentityAdoptsLegacyIPv6WithoutRelease 固定升级语义:
// 旧档案(current_ipv6 为空)首次观测到 IPv6 只采纳补写基线,不翻
// epoch、不释放仍有效的限制;采纳后真正的 IPv6 变化才翻篇。
func TestObserveExitIdentityAdoptsLegacyIPv6WithoutRelease(t *testing.T) {
	r := openIdentityRegistry(t)
	ctx := context.Background()
	// 模拟旧写入方:只落聚合列(v4),IPv6 列为空。
	if _, _, _, err := r.ObserveExitIdentity(ctx, 5, model.ExitIdentity{IPv4: "192.0.2.1"}, 1); err != nil {
		t.Fatal(err)
	}
	banCurrentExit(t, r, 5)
	old, current, released, err := r.ObserveExitIdentity(ctx, 5, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::9"}, 2)
	if err != nil || old != 0 || current != 0 || len(released) != 0 {
		t.Fatalf("adoption must not advance: old=%d current=%d released=%v err=%v", old, current, released, err)
	}
	if allowed, err := r.ExitAllowed(ctx, 5); err != nil || allowed {
		t.Fatalf("adoption released a live ban: %v %v", allowed, err)
	}
	record, ok, err := r.ExitIPAt(ctx, 5, 0)
	if err != nil || !ok || record.IPv6 != "2001:db8::9" {
		t.Fatalf("adoption must amend the baseline row: %+v ok=%v err=%v", record, ok, err)
	}
	// 采纳后的真实 IPv6 变化翻篇并释放。
	if _, current, released, err := r.ObserveExitIdentity(ctx, 5, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::10"}, 3); err != nil || current != 1 || len(released) != 1 {
		t.Fatalf("post-adoption change must advance and release: current=%d released=%v err=%v", current, released, err)
	}
}

// TestLegacySchemaSQLiteMigrationAddsIPv6Column 固定持久化兼容:
// 升级前的库(q_ip_epoch 无 current_ipv6 列,带旧行)Open 后自动加列,
// 旧行可读,采纳语义正常工作;失败可重试,不需人工数据操作。
func TestLegacySchemaSQLiteMigrationAddsIPv6Column(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := gorm.Open(glebarezsqlite.Open(path), qualityGormConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE q_ip_epoch (
		node_id integer NOT NULL,
		epoch integer NOT NULL DEFAULT 0,
		current_ip text NOT NULL DEFAULT '',
		first_seen_at datetime NOT NULL,
		changed_at datetime NOT NULL,
		PRIMARY KEY (node_id, epoch))`).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Exec("INSERT INTO q_ip_epoch (node_id, epoch, current_ip, first_seen_at, changed_at) VALUES (5, 0, ?, ?, ?)",
		"192.0.2.1", now, now).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()

	ctx := context.Background()
	r, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if r.CurrentEpoch(5) != 0 {
		t.Fatalf("legacy pointer lost: %d", r.CurrentEpoch(5))
	}
	// 首次双族观测:聚合未变 → 采纳,不翻篇。
	if _, current, released, err := r.ObserveExitIdentity(ctx, 5, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::9"}, 1); err != nil || current != 0 || len(released) != 0 {
		t.Fatalf("legacy row must adopt: current=%d released=%v err=%v", current, released, err)
	}
}
