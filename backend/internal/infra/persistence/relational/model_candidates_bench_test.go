package relational

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// seedRouteBench 建 1000 条跨 Provider 路由（含启用账号 binding——
// availableRoutes 谓词要求，缺失时候选查询按设计返回 not found）。
func seedRouteBench(b *testing.B, repo *ModelRepository, database *Database) []string {
	b.Helper()
	ctx := context.Background()
	providers := []account.Provider{account.ProviderBuild, account.ProviderWeb, account.ProviderConsole}
	const total = 1000
	names := make([]string, 0, total)
	for i := 0; i < total; i++ {
		names = append(names, fmt.Sprintf("grok-bench-%04d", i))
	}
	accountRepo := NewAccountRepository(database)
	syncedAt := time.Now().UTC()
	// 先建路由（UpsertDiscovered 按 Provider 命名空间），再为每个 Provider
	// 的账号建 binding——ReplaceAccountCapabilities 只绑已有路由，不建路由。
	for _, p := range providers {
		if err := repo.UpsertDiscovered(ctx, p, names); err != nil {
			b.Fatal(err)
		}
		seed := map[account.Provider]string{account.ProviderBuild: "build", account.ProviderWeb: "web", account.ProviderConsole: "console"}[p]
		created, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: p, Name: "bench-" + seed, SourceKey: "bench-" + seed,
			EncryptedAccessToken: "enc", Enabled: true, AuthStatus: account.AuthStatusActive,
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := repo.ReplaceAccountCapabilities(ctx, created.ID, names, syncedAt); err != nil {
			b.Fatal(err)
		}
	}
	return names
}

// BenchmarkGetByPublicIDCandidates 度量候选路由解析的每请求成本（所有
// /v1 请求的第一跳）。1000 路由 × 3 Provider 命名空间的最坏同名形态，
// 量化 IN+EXISTS+ORDER BY CASE 的规模表现（round 17 基线）。
//
// round 17 实测基线（i9-13980HX 容器）：Candidates 6.0ms/op、306 allocs；
// HasEnabled 1.2ms/op。分解（TestPredicateCostSplit，已删的临时诊断）：
// 裸 find≈1.0ms，谓词放大 5.5×。EXPLAIN 证实 public_id IN 走 covering
// index——成本大头是 ORDER BY CASE 全候选排序 + GORM 行映射，非全表扫。
// 当前生产路由表 ~14 行时该查询 <100µs；本基线的作用是路由表增长后
// 有对照数字。若未来路由过千且 QPS 升高，优化方向：候选集 LIMIT +
// 应用层排序（谓词已索引友好）。
func BenchmarkGetByPublicIDCandidates(b *testing.B) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, b.TempDir()+"/candidates-bench.db")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	repo := NewModelRepository(database)
	names := seedRouteBench(b, repo, database)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := repo.GetByPublicIDCandidates(ctx, names[i%len(names)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHasEnabledRouteByPublicID 度量消歧存在性查询（仅在候选 miss
// 的 404/503 路径触发）同表规模下的单次成本，确认低频路径开销有界。
func BenchmarkHasEnabledRouteByPublicID(b *testing.B) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, b.TempDir()+"/hasenabled-bench.db")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	repo := NewModelRepository(database)
	names := seedRouteBench(b, repo, database)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := repo.HasEnabledRouteByPublicID(ctx, names[i%len(names)]); err != nil {
			b.Fatal(err)
		}
	}
}
