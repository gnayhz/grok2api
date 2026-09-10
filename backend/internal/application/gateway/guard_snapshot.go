package gateway

import (
	"errors"

	guardpolicy "github.com/chenyme/grok2api/backend/internal/domain/guard"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// GuardSnapshot is the complete request policy supplied by one authority.
// Kernels must be immutable and safe for concurrent calls. A missing kernel
// when protection is enabled is unavailable, never an implicit bypass.
type GuardSnapshot struct {
	Runtime      QualityRetryRuntime
	PathResolver attemptmeta.PathResolver
	Kernel       QualityHoldKernel
	Err          error
}

type GuardSnapshotSource interface{ GuardSnapshot() GuardSnapshot }
type guardSnapshotSource struct{ source GuardSnapshotSource }

func (s *Service) SetGuardSnapshotSource(source GuardSnapshotSource) {
	if source == nil {
		s.guardSource.Store(nil)
		return
	}
	s.guardSource.Store(&guardSnapshotSource{source: source})
}

type snapshotJurisdiction []string

func (models snapshotJurisdiction) Jurisdiction(provider, model string) bool {
	return (guardpolicy.Config{GuardedModels: models}).Jurisdiction(provider, model)
}

func (s *Service) requestGuardSnapshot() (QualityRetryRuntime, QualityJurisdiction) {
	if source := s.guardSource.Load(); source != nil {
		snapshot := source.source.GuardSnapshot()
		cfg := snapshot.Runtime
		cfg.OnExhausted = (guardpolicy.Config{}).ExhaustionPolicy()
		cfg.GuardedModels = append([]string(nil), cfg.GuardedModels...)
		cfg.kernel, cfg.unavailable = snapshot.Kernel, snapshot.Err
		cfg.pathResolver = snapshot.PathResolver
		if cfg.unavailable == nil {
			// An authority snapshot is complete and must already be valid.
			// Do not silently normalize broken policy on the request path.
			cfg.unavailable = guardpolicy.Validate(guardpolicy.Config{
				Enabled: cfg.Enabled, MaxAttempts: cfg.MaxAttempts, GuardedModels: cfg.GuardedModels,
				EvidenceTimeout: cfg.EvidenceTimeout, CreatedTimeout: cfg.CreatedTimeout,
				AdmissionTimeout: cfg.AdmissionTimeout, ToolAdmissionTimeout: cfg.ToolAdmissionTimeout,
				AccountCooldown: cfg.AccountCooldown, IdleAccountCooldown: cfg.IdleAccountCooldown,
			})
		}

		if cfg.Enabled && cfg.kernel == nil {
			cfg.unavailable = errors.New("guard kernel unavailable")
		}
		return cfg, snapshotJurisdiction(cfg.GuardedModels)
	}
	// Standalone gateways use an immutable built-in kernel. The optional
	// jurisdiction seam remains for compatibility with embedded callers.
	cfg := s.qualityRetryConfig()
	cfg.kernel = builtinQualityKernel{}
	return cfg, s.modelJurisdictionObserver()
}
