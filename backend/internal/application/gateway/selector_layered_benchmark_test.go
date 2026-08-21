package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func BenchmarkSelectorMultiModelCandidateLoad(b *testing.B) {
	const accountCount = 300
	models := []string{
		"benchmark-model-1", "benchmark-model-2", "benchmark-model-3", "benchmark-model-4",
		"benchmark-model-5", "benchmark-model-6", "benchmark-model-7", "benchmark-model-8",
	}
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "selector-layered-benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	routes := relational.NewModelRepository(database)
	credentials := make([]account.Credential, accountCount)
	for index := range credentials {
		credentials[index] = account.Credential{
			Provider: account.ProviderBuild, Name: fmt.Sprintf("benchmark-%04d", index),
			SourceKey: fmt.Sprintf("benchmark-source-%04d", index), EncryptedAccessToken: "encrypted",
			AuthStatus: account.AuthStatusActive, Priority: account.DefaultPriority, MaxConcurrent: account.DefaultMaxConcurrent,
		}
	}
	created, err := accounts.UpsertManyByIdentity(ctx, credentials)
	if err != nil {
		b.Fatal(err)
	}
	syncedAt := time.Now().UTC()
	for _, value := range created {
		if err := routes.ReplaceAccountCapabilities(ctx, value.ID, models, syncedAt); err != nil {
			b.Fatal(err)
		}
	}

	for _, modelCount := range []int{2, 8} {
		b.Run(fmt.Sprintf("models_%d", modelCount), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				selector := NewSelector(accounts, nil, nil, nil, time.Hour, time.Second, time.Minute)
				for _, upstreamModel := range models[:modelCount] {
					candidates, loadErr := selector.loadCandidates(ctx, account.ProviderBuild, 0, upstreamModel, "", time.Now().UTC())
					if loadErr != nil {
						b.Fatal(loadErr)
					}
					if len(candidates) != accountCount {
						b.Fatalf("candidates = %d, want %d", len(candidates), accountCount)
					}
				}
			}
		})
	}
}

// BenchmarkSelectorCandidateCacheHit 度量生产热路径的真实每请求成本：
// 快照缓存命中（固定 now < expiresAt，免 30s TTL 失效）。这是选号在每个
// 请求上实际发生的事——cold-load（上方 models_N 基准）只在 TTL 过期后
// 每 30s 一次，且被 singleflight 合并。区分两者避免把冷启动成本误当
// 每请求成本（2026-08-21 轮4甄别记录）。
func BenchmarkSelectorCandidateCacheHit(b *testing.B) {
	const accountCount = 300
	models := []string{"benchmark-model-1", "benchmark-model-2"}
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "selector-cache-hit.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	routes := relational.NewModelRepository(database)
	credentials := make([]account.Credential, accountCount)
	for index := range credentials {
		credentials[index] = account.Credential{
			Provider: account.ProviderBuild, Name: fmt.Sprintf("benchmark-%04d", index),
			SourceKey: fmt.Sprintf("benchmark-source-%04d", index), EncryptedAccessToken: "encrypted",
			AuthStatus: account.AuthStatusActive, Priority: account.DefaultPriority, MaxConcurrent: account.DefaultMaxConcurrent,
		}
	}
	created, err := accounts.UpsertManyByIdentity(ctx, credentials)
	if err != nil {
		b.Fatal(err)
	}
	syncedAt := time.Now().UTC()
	for _, value := range created {
		if err := routes.ReplaceAccountCapabilities(ctx, value.ID, models, syncedAt); err != nil {
			b.Fatal(err)
		}
	}
	selector := NewSelector(accounts, nil, nil, nil, time.Hour, time.Second, time.Minute)
	// 固定时间戳预热一次缓存；后续迭代同一 now 命中快照。
	now := time.Now().UTC()
	for _, upstreamModel := range models {
		if _, err := selector.loadCandidates(ctx, account.ProviderBuild, 0, upstreamModel, "", now); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for _, upstreamModel := range models {
			if _, err := selector.loadCandidates(ctx, account.ProviderBuild, 0, upstreamModel, "", now); err != nil {
				b.Fatal(err)
			}
		}
	}
}
