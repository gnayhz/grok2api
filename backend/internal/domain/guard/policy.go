// Package guard owns admission policy values, defaults and validity. It has no storage or transport dependencies.
package guard

import (
	"errors"
	"fmt"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

const (
	DefaultMaxAttempts          = 2
	DefaultEvidenceTimeout      = 3500 * time.Millisecond
	DefaultCreatedTimeout       = 5 * time.Second
	DefaultAdmissionTimeout     = 30 * time.Second
	DefaultToolAdmissionTimeout = 3 * time.Minute
	DefaultAccountCooldown      = 2 * time.Minute
	DefaultIdleAccountCooldown  = accountdomain.DefaultQualityIdleCooldown
)

// Config 是守卫配置(G13 管辖勾选;预算;fail-closed 唯一)。
type Config struct {
	// Revision identifies a persisted policy. Zero denotes the file baseline.
	Revision uint64
	// All admission policy lives in this record; legacy requestRetry is a projection.
	EvidenceTimeout      time.Duration
	CreatedTimeout       time.Duration
	AdmissionTimeout     time.Duration
	ToolAdmissionTimeout time.Duration
	AccountCooldown      time.Duration
	IdleAccountCooldown  time.Duration

	// Enabled 守卫总开关。
	Enabled bool
	// GuardedModels 管辖模型清单(用户勾选,G13;空=无管辖)。
	GuardedModels []string
	// MaxAttempts 每请求降智换号预算。
	MaxAttempts int
	// ReasoningExpected is retained for persisted settings compatibility.
	// Business requests derive their thinking expectation from resolved effort.
	ReasoningExpected bool
}

// DefaultConfig 默认管辖沿用现行白名单(面板可改)。
func DefaultConfig() Config {
	return Normalize(Config{Enabled: true, GuardedModels: []string{"grok-4.5", "grok-4.6"}, MaxAttempts: DefaultMaxAttempts, ReasoningExpected: true})
}

// Jurisdiction 报告模型是否在管辖内(G13;条目=裸公开名[任意渠道]
// 或 "渠道:公开名"[限定渠道],渠道取值 grok_build/grok_console/grok_web)。
func (c Config) Jurisdiction(provider, model string) bool {
	for _, guarded := range c.GuardedModels {
		channel, name, scoped := strings.Cut(guarded, ":")
		if (!scoped && guarded == model) || (scoped && channel == provider && name == model) {
			return true
		}
	}
	return false
}

// ExhaustionPolicy 返回耗尽策略——恒 fail-closed(G12:唯一,无选项)。
func (c Config) ExhaustionPolicy() string { return "fail_closed" }

func normalizeGuardedModels(models []string) []string {
	normalized := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model)
		if name != "" && !seen[name] {
			normalized = append(normalized, name)
			seen[name] = true
		}
	}
	return normalized
}

// Normalize owns the model slice and fills zero durations before validation.
func Normalize(cfg Config) Config {
	cfg.GuardedModels = normalizeGuardedModels(cfg.GuardedModels)
	if cfg.EvidenceTimeout == 0 {
		cfg.EvidenceTimeout = DefaultEvidenceTimeout
	}
	if cfg.CreatedTimeout == 0 {
		cfg.CreatedTimeout = DefaultCreatedTimeout
	}
	if cfg.AdmissionTimeout == 0 {
		cfg.AdmissionTimeout = DefaultAdmissionTimeout
	}
	if cfg.ToolAdmissionTimeout == 0 {
		cfg.ToolAdmissionTimeout = DefaultToolAdmissionTimeout
	}
	if cfg.AccountCooldown == 0 {
		cfg.AccountCooldown = DefaultAccountCooldown
	}
	if cfg.IdleAccountCooldown == 0 {
		cfg.IdleAccountCooldown = DefaultIdleAccountCooldown
	}
	return cfg
}

var ErrInvalidInput = errors.New("invalid guard configuration")
var ErrInvalidJurisdiction = errors.New("invalid jurisdiction entry")

// Validate checks a complete policy, including explicitly disabled policies.
func Validate(cfg Config) error {
	if cfg.Enabled && len(cfg.GuardedModels) == 0 {
		return errors.New("guard: enabled policy requires at least one model")
	}
	for _, entry := range cfg.GuardedModels {
		if strings.TrimSpace(entry) == "" {
			return ErrInvalidJurisdiction
		}
		provider, model, scoped := strings.Cut(entry, ":")
		if scoped && (model == "" || (provider != "grok_build" && provider != "grok_console" && provider != "grok_web")) {
			return ErrInvalidJurisdiction
		}
	}

	if cfg.MaxAttempts < 1 || cfg.MaxAttempts > 100 {
		return errors.New("guard: attempt budget must be between 1 and 100")
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"createdTimeout", cfg.CreatedTimeout}, {"evidenceTimeout", cfg.EvidenceTimeout},
		{"admissionTimeout", cfg.AdmissionTimeout}, {"toolAdmissionTimeout", cfg.ToolAdmissionTimeout},
	} {
		if field.value <= 0 || field.value > 24*time.Hour {
			return fmt.Errorf("guard: %s must be positive and at most 24 hours", field.name)
		}
	}
	// Preserve the file configuration's supported cooldown window. Waiting
	// budgets and account restriction lifetimes are different policy dimensions.
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"accountCooldown", cfg.AccountCooldown}, {"idleAccountCooldown", cfg.IdleAccountCooldown},
	} {
		if field.value <= 0 || field.value > 168*time.Hour {
			return fmt.Errorf("guard: %s must be positive and at most 168 hours", field.name)
		}
	}
	return nil
}

// Bootstrap preserves the legacy file/old gateway document convention: a zero
// attempt count and an empty model list mean the built-in defaults. Management
// policies use Normalize directly so an explicit empty, disabled scope stays empty.
func Bootstrap(cfg Config) Config {
	defaults := DefaultConfig()
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = defaults.MaxAttempts
	}
	if len(cfg.GuardedModels) == 0 {
		cfg.GuardedModels = defaults.GuardedModels
	}
	cfg.ReasoningExpected = defaults.ReasoningExpected
	return Normalize(cfg)
}
