package admission

import (
	"time"

	guardpolicy "github.com/chenyme/grok2api/backend/internal/domain/guard"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

type QualityRetryRuntime struct {
	Revision             uint64
	RuleVersion          string
	AdmissionTimeout     time.Duration
	ToolAdmissionTimeout time.Duration
	kernel               QualityHoldKernel
	pathResolver         attemptmeta.PathResolver
	unavailable          error

	Enabled             bool
	MaxAttempts         int
	OnExhausted         string
	AccountCooldown     time.Duration
	IdleAccountCooldown time.Duration
	EvidenceTimeout     time.Duration
	CreatedTimeout      time.Duration
	ReasoningExpected   bool
	GuardedModels       []string
}

func (cfg QualityRetryRuntime) Kernel() QualityHoldKernel              { return cfg.kernel }
func (cfg *QualityRetryRuntime) SetKernel(k QualityHoldKernel)         { cfg.kernel = k }
func (cfg QualityRetryRuntime) PathResolver() attemptmeta.PathResolver { return cfg.pathResolver }
func (cfg *QualityRetryRuntime) SetPathResolver(r attemptmeta.PathResolver) {
	cfg.pathResolver = r
}
func (cfg QualityRetryRuntime) Unavailable() error        { return cfg.unavailable }
func (cfg *QualityRetryRuntime) SetUnavailable(err error) { cfg.unavailable = err }

func NormalizeRuntime(cfg QualityRetryRuntime) QualityRetryRuntime {
	if cfg.AdmissionTimeout <= 0 {
		cfg.AdmissionTimeout = guardpolicy.DefaultAdmissionTimeout
	}
	if cfg.ToolAdmissionTimeout <= 0 {
		cfg.ToolAdmissionTimeout = guardpolicy.DefaultToolAdmissionTimeout
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = guardpolicy.DefaultMaxAttempts
	}
	if cfg.AccountCooldown <= 0 {
		cfg.AccountCooldown = guardpolicy.DefaultAccountCooldown
	}
	if cfg.IdleAccountCooldown <= 0 {
		cfg.IdleAccountCooldown = guardpolicy.DefaultIdleAccountCooldown
	}
	if cfg.EvidenceTimeout <= 0 {
		cfg.EvidenceTimeout = guardpolicy.DefaultEvidenceTimeout
	}
	if cfg.CreatedTimeout <= 0 {
		cfg.CreatedTimeout = guardpolicy.DefaultCreatedTimeout
	}
	cfg.OnExhausted = "fail_closed"
	return cfg
}

type QualityHoldKernel interface {
	ClassifyQualityHold(sig QualityStreamSignals) QualityVerdict
}

type QualityStreamSignals struct {
	HasThinking                   bool
	ReasoningEndedWithoutThinking bool
	VisibleTokens                 int64
	ReasoningTokens               int64
	OutputTokens                  int64
	Terminal                      bool
}

type QualityJurisdiction interface {
	Jurisdiction(provider, model string) bool
}

type GuardSnapshot struct {
	Runtime      QualityRetryRuntime
	PathResolver attemptmeta.PathResolver
	Kernel       QualityHoldKernel
	Err          error
}

type GuardSnapshotSource interface{ GuardSnapshot() GuardSnapshot }

type ModelJurisdiction []string

func (models ModelJurisdiction) Jurisdiction(provider, model string) bool {
	return (guardpolicy.Config{GuardedModels: models}).Jurisdiction(provider, model)
}

func ApplySnapshot(snapshot GuardSnapshot) (QualityRetryRuntime, QualityJurisdiction) {
	cfg := snapshot.Runtime
	cfg.OnExhausted = (guardpolicy.Config{}).ExhaustionPolicy()
	cfg.GuardedModels = append([]string(nil), cfg.GuardedModels...)
	cfg.SetKernel(snapshot.Kernel)
	cfg.SetUnavailable(snapshot.Err)
	cfg.SetPathResolver(snapshot.PathResolver)
	if cfg.Unavailable() == nil {
		cfg.SetUnavailable(guardpolicy.Validate(guardpolicy.Config{
			Enabled: cfg.Enabled, MaxAttempts: cfg.MaxAttempts, GuardedModels: cfg.GuardedModels,
			EvidenceTimeout: cfg.EvidenceTimeout, CreatedTimeout: cfg.CreatedTimeout,
			AdmissionTimeout: cfg.AdmissionTimeout, ToolAdmissionTimeout: cfg.ToolAdmissionTimeout,
			AccountCooldown: cfg.AccountCooldown, IdleAccountCooldown: cfg.IdleAccountCooldown,
		}))
	}
	if cfg.Enabled && cfg.Kernel() == nil {
		cfg.SetUnavailable(errKernelUnavailable)
	}
	return cfg, ModelJurisdiction(cfg.GuardedModels)
}
