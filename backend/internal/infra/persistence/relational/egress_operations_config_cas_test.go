package relational

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// openOperationsConfigCASDatabase 默认 SQLite;配置 TEST_POSTGRES_DSN 时切换
// PostgreSQL——条件写依赖 timestamptz 的 UpdatedAt 往返精度与 FOR UPDATE 行锁,
// 必须在真实 PG 上回归,而不是只依赖 SQLite 的容错。
func openOperationsConfigCASDatabase(t *testing.T) *Database {
	t.Helper()
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		database, err := OpenPostgres(ctx, dsn, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.InitializeSchema(ctx); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
		// 测试套件共享同一个临时 PG 库;legacy 升级测试假设自己是第一个写入
		// egress_operations_config 主键 1 的测试(其旧库种子直接 INSERT id=1)。
		// 先注册 Close(LIFO 后执行),再注册行清理,退出时恢复该前提。
		t.Cleanup(func() { _ = database.Close() })
		t.Cleanup(func() { _ = database.db.Exec("DELETE FROM egress_operations_config").Error })
		return database
	}
	database, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "ops-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(context.Background()); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func seedOperationsConfigRow(t *testing.T, repo *EgressRepository) egress.OperationsConfig {
	t.Helper()
	if _, err := repo.SaveEgressOperationsConfig(context.Background(), egress.OperationsConfig{
		ProbeProvider: egress.ProbeProviderCloudflare, ProbeIntervalSeconds: 900,
		DefaultTarget: egress.RoutingTarget{Mode: egress.RoutingTargetAuto},
		ScopeTargets: map[egress.Scope]egress.RoutingTarget{
			egress.ScopeBuild: {Mode: egress.RoutingTargetDirect},
		},
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// CAS 令牌必须来自读取:Save 的返回值是内存构造行,其 UpdatedAt 未经
	// 存储往返(驱动精度截断),直接用作令牌会误判 stale。
	snapshot, err := repo.GetEgressOperationsConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// 旧快照不得覆盖并发提交:快照读取之后配置被其他写入者修改,条件写必须以
// ErrEgressConfigStale 拒绝,且拒绝不得改动现有行。
func TestSaveEgressOperationsConfigIfCurrentRejectsStaleSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := NewEgressRepository(openOperationsConfigCASDatabase(t))

	snapshot := seedOperationsConfigRow(t, repo)

	admin := snapshot
	admin.ProbeIntervalSeconds = 120
	admin.UpdatedAt = time.Now().UTC()
	if _, err := repo.SaveEgressOperationsConfig(ctx, admin); err != nil {
		t.Fatal(err)
	}

	stale := snapshot
	delete(stale.ScopeTargets, egress.ScopeBuild)
	_, err := repo.SaveEgressOperationsConfigIfCurrent(ctx, stale, snapshot.UpdatedAt)
	if !errors.Is(err, repository.ErrEgressConfigStale) {
		t.Fatalf("stale snapshot write must be rejected with ErrEgressConfigStale, got %v", err)
	}

	final, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if final.ProbeIntervalSeconds != 120 {
		t.Fatalf("rejected write must not modify the row: interval=%d, want 120", final.ProbeIntervalSeconds)
	}
	if _, ok := final.ScopeTargets[egress.ScopeBuild]; !ok {
		t.Fatal("rejected write must not strip the scope target")
	}
}

// 快照仍然最新时条件写必须成功,并只提交调用方的目标变更;写后旧时间戳
// 立即失效。
func TestSaveEgressOperationsConfigIfCurrentCommitsWhenCurrent(t *testing.T) {
	ctx := context.Background()
	repo := NewEgressRepository(openOperationsConfigCASDatabase(t))

	snapshot := seedOperationsConfigRow(t, repo)

	current := snapshot
	delete(current.ScopeTargets, egress.ScopeBuild)
	saved, err := repo.SaveEgressOperationsConfigIfCurrent(ctx, current, snapshot.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.ScopeTargets[egress.ScopeBuild]; ok {
		t.Fatalf("scope target must be stripped by the CAS save: %+v", saved.ScopeTargets)
	}

	retry := snapshot
	retry.ProbeIntervalSeconds = 600
	if _, err := repo.SaveEgressOperationsConfigIfCurrent(ctx, retry, snapshot.UpdatedAt); !errors.Is(err, repository.ErrEgressConfigStale) {
		t.Fatalf("post-commit write with the old snapshot timestamp must be stale, got %v", err)
	}

	final, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if final.ProbeIntervalSeconds != 900 {
		t.Fatalf("rejected retry must not modify the row: interval=%d, want 900", final.ProbeIntervalSeconds)
	}
}
