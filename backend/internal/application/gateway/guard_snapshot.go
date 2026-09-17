package gateway

import (
	admpkg "github.com/chenyme/grok2api/backend/internal/application/admission"
)

type guardSnapshotSource struct{ source GuardSnapshotSource }

// SetGuardSnapshotSource 安装守卫配置快照源:生产组合根注入随设置热更的
// 动态源;测试与固定基线场景可使用 StaticGuardSnapshotSource。
func (s *Service) SetGuardSnapshotSource(source GuardSnapshotSource) {
	if source == nil {
		s.guardSource.Store(nil)
		return
	}
	s.guardSource.Store(&guardSnapshotSource{source: source})
}

// StaticGuardSnapshotSource 返回恒定快照的守卫配置源。构造时先经
// normalizeQualityRetry 补齐缺省值并对切片字段做防御性拷贝(调用方
// 后续修改不影响快照)。快照统一经 admpkg.ApplySnapshot 校验并派生
// 管辖清单,不存在第二条配置入口。
func StaticGuardSnapshotSource(cfg QualityRetryRuntime) GuardSnapshotSource {
	cfg = normalizeQualityRetry(cfg)
	cfg.GuardedModels = append([]string(nil), cfg.GuardedModels...)
	return staticGuardSnapshotSource{cfg: cfg}
}

type staticGuardSnapshotSource struct{ cfg QualityRetryRuntime }

func (s staticGuardSnapshotSource) GuardSnapshot() GuardSnapshot {
	return GuardSnapshot{Runtime: s.cfg, Kernel: builtinQualityKernel{}}
}

// requestGuardSnapshot 是守卫配置的唯一读取点:有源走快照校验路径,
// 无源(裸构造的 Service,仅测试)回到进程内建默认(守卫关闭)。
func (s *Service) requestGuardSnapshot() (QualityRetryRuntime, QualityJurisdiction) {
	if source := s.guardSource.Load(); source != nil {
		cfg, jurisdiction := admpkg.ApplySnapshot(source.source.GuardSnapshot())
		return cfg, jurisdiction
	}
	cfg := normalizeQualityRetry(QualityRetryRuntime{})
	cfg.SetKernel(builtinQualityKernel{})
	return cfg, admpkg.ModelJurisdiction(cfg.GuardedModels)
}
