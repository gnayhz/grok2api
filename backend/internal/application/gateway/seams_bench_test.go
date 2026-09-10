package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// benchmarkSeamDB 预置 32 个可调度账号(批2 热路径基准:资格谓词缝隙
// 在候选过滤循环中的每选择成本)。
func benchmarkSeamDB(b *testing.B) (*Selector, func()) {
	b.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "seam-bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	for i := 0; i < 32; i++ {
		name := "bench-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100 - i, MaxConcurrent: 64,
		}); err != nil {
			b.Fatal(err)
		}
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	return selector, func() { _ = database.Close() }
}

func benchmarkSeamAcquire(b *testing.B, withSeam bool) {
	selector, cleanup := benchmarkSeamDB(b)
	defer cleanup()
	if withSeam {
		selector.SetQualityEligibility(stubAccountEligibility{ineligible: map[uint64]bool{}})
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true)
		if err != nil {
			b.Fatal(err)
		}
		lease.Release()
	}
}

// BenchmarkSelectorAcquireNilSeam 缝隙未注入(剥离态)的选择成本。
func BenchmarkSelectorAcquireNilSeam(b *testing.B) { benchmarkSeamAcquire(b, false) }

// BenchmarkSelectorAcquireEligibilitySeam 缝隙注入(恒真谓词)的选择成本:
// 与 NilSeam 的差值即缝隙本身的边际成本(原子读+分支/候选)。
func BenchmarkSelectorAcquireEligibilitySeam(b *testing.B) { benchmarkSeamAcquire(b, true) }
