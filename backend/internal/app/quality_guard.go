package app

import (
	"context"
	"log/slog"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// qualityHoldKernel 适配 gateway.QualityHoldKernel → guard.Judge
// (B4 决议2 依赖倒置的注入侧:接口在底座,实现在质量层,组合根连接)。
type qualityHoldKernel struct{}

func (qualityHoldKernel) ClassifyQualityHold(sig gateway.QualityStreamSignals) gateway.QualityVerdict {
	verdict, _ := qualityguard.Judge(qualityguard.Signals{
		HasThinking:                   sig.HasThinking,
		ReasoningEndedWithoutThinking: sig.ReasoningEndedWithoutThinking,
		VisibleTokens:                 sig.VisibleTokens,
		OutputTokens:                  sig.OutputTokens,
		Terminal:                      sig.Terminal,
	})
	switch verdict {
	case qualityguard.Deliver:
		return gateway.QualityDeliver
	case qualityguard.Withhold:
		return gateway.QualityWithhold
	default:
		return gateway.QualityWait
	}
}

// The file decoder delegates bootstrap semantics to the domain policy.
func qualityGuardConfig(value config.RequestRetryConfig) qualityguard.Config {
	return value.GuardPolicy()
}

// bootstrapGuardService loads the instance's authoritative guard configuration;
// absence uses the file baseline and load failure makes snapshots unavailable.
func bootstrapGuardService(ctx context.Context, documents repository.SettingsDocumentRepository, logger *slog.Logger, initial, fileBase qualityguard.Config) *qualityguard.Service {
	service := qualityguard.NewWithFileDefaults(initial, fileBase, qualityguard.NewDocumentStore(documents))
	if err := service.LoadPersisted(ctx); err != nil {
		// 读失败不阻断构造；Snapshot 标记 unavailable，使请求拒绝旁路。
		logger.Error("quality_guard_persisted_load_failed", "error", err)
	}
	return service
}

// qualityGuardSnapshotSource is the instance-scoped production dependency.
type qualityGuardSnapshotSource struct {
	service  *qualityguard.Service
	registry *qualityregistry.Registry
}

func (s qualityGuardSnapshotSource) GuardSnapshot() gateway.GuardSnapshot {
	cfg, err := s.service.Snapshot()
	var pathResolver attemptmeta.PathResolver
	if s.registry != nil {
		pathResolver = s.registry
	}
	return gateway.GuardSnapshot{Runtime: gateway.QualityRetryRuntime{
		Revision: cfg.Revision, RuleVersion: qualityguard.RuleVersion,
		Enabled: cfg.Enabled, MaxAttempts: cfg.MaxAttempts, GuardedModels: cfg.GuardedModels,
		ReasoningExpected: cfg.ReasoningExpected,
		EvidenceTimeout:   cfg.EvidenceTimeout, CreatedTimeout: cfg.CreatedTimeout,
		AdmissionTimeout: cfg.AdmissionTimeout, ToolAdmissionTimeout: cfg.ToolAdmissionTimeout,
		AccountCooldown: cfg.AccountCooldown, IdleAccountCooldown: cfg.IdleAccountCooldown,
	}, Kernel: qualityHoldKernel{}, PathResolver: pathResolver, Err: err}
}
