package registry

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// 热路径基准(B2 性能约束:谓词 O(1) 内存可判定)。

func benchRegistry(b *testing.B) *Registry {
	registry, err := Open(context.Background(), Options{Driver: "sqlite", SQLitePath: filepath.Join(b.TempDir(), "bench.db")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = registry.Close() })
	// 预置 1000 个羁押账号 + 500 个 ban 出口,模拟有状态的缓存规模。
	ctx := context.Background()
	for i := 1; i <= 1000; i++ {
		if err := registry.TransitionAccount(ctx, AccountTransitionRequest{AccountID: uint64(i), To: model.AccountRemanded, CaseID: uint64(i)}); err != nil {
			b.Fatal(err)
		}
	}
	for i := 1; i <= 500; i++ {
		if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: uint64(i), To: model.ExitRemanded, CaseID: uint64(i)}); err != nil {
			b.Fatal(err)
		}
		if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: uint64(i), To: model.ExitBanned, CaseID: uint64(i)}); err != nil {
			b.Fatal(err)
		}
	}
	return registry
}

func BenchmarkAccountEligible(b *testing.B) {
	registry := benchRegistry(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if registry.AccountEligible(uint64(i % 1500)) {
			_ = i
		}
	}
}

func BenchmarkExitEligible(b *testing.B) {
	registry := benchRegistry(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if registry.ExitEligible(uint64(i % 700)) {
			_ = i
		}
	}
}

func BenchmarkIdentityGroupOf(b *testing.B) {
	registry := benchRegistry(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = registry.IdentityGroupOf(uint64(i % 100))
	}
}
