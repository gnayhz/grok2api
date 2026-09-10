package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"
)

func openGuardTestDatabase(t *testing.T) *relational.Database {
	t.Helper()
	db, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestQualityGuardStoreRoundtrip 守卫配置持久化(切换手册第1步):
// 落库→读回→覆盖→读回,自有行(key=quality_guard)与底座行互不影响。
func TestQualityGuardStoreRoundtrip(t *testing.T) {
	database := openGuardTestDatabase(t)
	ctx := context.Background()
	store := qualityguard.NewDocumentStore(relational.NewSettingsDocumentRepository(database, qualityguard.SettingsKey))

	if _, found, err := store.LoadGuard(ctx); err != nil || found {
		t.Fatalf("空库读回应为未找到: found=%v err=%v", found, err)
	}

	want := qualityguard.Config{Revision: 1,
		Enabled: true, GuardedModels: []string{"grok-4.5", "grok-4.6"},
		MaxAttempts: 3, ReasoningExpected: true,
	}
	if err := store.SaveGuard(ctx, want); err != nil {
		t.Fatalf("首次落库: %v", err)
	}
	got, found, err := store.LoadGuard(ctx)
	if err != nil || !found {
		t.Fatalf("落库后读回: found=%v err=%v", found, err)
	}
	if got.MaxAttempts != 3 || len(got.GuardedModels) != 2 || !got.Enabled || !got.ReasoningExpected {
		t.Fatalf("读回不一致: %+v", got)
	}

	// 覆盖保存(upsert 路径)。
	want.Revision = 2
	want.MaxAttempts = 5
	want.GuardedModels = []string{"grok-4.7"}
	if err := store.SaveGuard(ctx, want); err != nil {
		t.Fatalf("覆盖落库: %v", err)
	}
	got, _, _ = store.LoadGuard(ctx)
	if got.MaxAttempts != 5 || len(got.GuardedModels) != 1 || got.GuardedModels[0] != "grok-4.7" {
		t.Fatalf("覆盖后读回不一致: %+v", got)
	}

	// 底座行(key=gateway)不受质量层写入影响。
	gatewayDoc, err := relational.NewSettingsDocumentRepository(database, "gateway").Load(ctx)
	if err != nil || gatewayDoc.Revision != 0 {
		t.Fatalf("guard wrote gateway: %v %+v", err, gatewayDoc)
	}

}

// TestBootstrapGuardServiceLoadsPersisted 启动读回持久化配置。
func TestBootstrapGuardServiceLoadsPersisted(t *testing.T) {
	database := openGuardTestDatabase(t)
	ctx := context.Background()
	store := qualityguard.NewDocumentStore(relational.NewSettingsDocumentRepository(database, qualityguard.SettingsKey))
	persisted := qualityguard.DefaultConfig()
	persisted.Revision = 1
	persisted.MaxAttempts = 7
	if err := store.SaveGuard(ctx, persisted); err != nil {
		t.Fatalf("预置持久化配置: %v", err)
	}
	service := qualityguard.New(qualityguard.DefaultConfig(), store)
	if err := service.LoadPersisted(ctx); err != nil {
		t.Fatalf("启动读回: %v", err)
	}
	if got := service.Config().MaxAttempts; got != 7 {
		t.Fatalf("启动未读到持久化配置: %d", got)
	}
}

// TestBootstrapGuardServiceUsesBaseWithoutPersistedRow 锚定配置继承:
// 没有质量层覆盖行时,守卫必须从文件基线启动,不能偷偷套硬编码默认值。
func TestBootstrapGuardServiceUsesBaseWithoutPersistedRow(t *testing.T) {
	database := openGuardTestDatabase(t)
	base := qualityguard.Config{
		Enabled: false, GuardedModels: []string{"grok-custom"}, MaxAttempts: 7,
	}
	service := bootstrapGuardService(context.Background(), relational.NewSettingsDocumentRepository(database, qualityguard.SettingsKey), slog.Default(), base, base)
	got := service.Config()
	if got.Enabled || got.MaxAttempts != base.MaxAttempts || len(got.GuardedModels) != 1 || got.GuardedModels[0] != "grok-custom" {
		t.Fatalf("无覆盖行时应沿用文件基线, got %+v", got)
	}
}

func TestGuardStoreRejectsStaleReplicaAndPreservesAuthority(t *testing.T) {
	database := openGuardTestDatabase(t)
	store := qualityguard.NewDocumentStore(relational.NewSettingsDocumentRepository(database, qualityguard.SettingsKey))
	first := qualityguard.New(qualityguard.DefaultConfig(), store)
	second := qualityguard.New(qualityguard.DefaultConfig(), store)
	cfg := first.Config()
	cfg.MaxAttempts = 4
	if _, err := first.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	stale := second.Config()
	stale.MaxAttempts = 8
	if _, err := second.Update(context.Background(), stale); !errors.Is(err, qualityguard.ErrConflict) {
		t.Fatalf("stale replica: %v", err)
	}
	if err := second.LoadPersisted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := second.Config(); got.Revision != 1 || got.MaxAttempts != 4 {
		t.Fatalf("lost policy: %+v", got)
	}
	stale = second.Config()
	stale.MaxAttempts = 3
	if _, err := second.Update(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Update(context.Background(), first.Config()); !errors.Is(err, qualityguard.ErrConflict) {
		t.Fatalf("old authority overwrote: %v", err)
	}
}

func TestGuardLegacyFallbackAndActualFileResetAreSeparate(t *testing.T) {
	database := openGuardTestDatabase(t)
	documents := relational.NewSettingsDocumentRepository(database, qualityguard.SettingsKey)
	file := qualityguard.DefaultConfig()
	file.MaxAttempts = 2
	file.AccountCooldown = 6 * time.Minute
	legacy := file
	legacy.MaxAttempts = 3
	service := bootstrapGuardService(context.Background(), documents, slog.Default(), legacy, file)
	if service.Config().MaxAttempts != 3 || service.FileDefaults().MaxAttempts != 2 {
		t.Fatal("legacy fallback overwrote file defaults")
	}
	restored, err := service.ResetToDefaults(context.Background(), 0)
	if err != nil || restored.Revision != 1 || restored.MaxAttempts != 2 {
		t.Fatalf("file reset=%+v %v", restored, err)
	}
	restarted := bootstrapGuardService(context.Background(), documents, slog.Default(), legacy, file)
	if restarted.Config().MaxAttempts != 2 || restarted.Config().Revision != 1 {
		t.Fatal("persisted file reset did not override legacy")
	}
	defaults := restarted.FileDefaults()
	defaults.GuardedModels[0] = "changed externally"
	if restarted.FileDefaults().GuardedModels[0] == defaults.GuardedModels[0] {
		t.Fatal("mutable file baseline")
	}
}
