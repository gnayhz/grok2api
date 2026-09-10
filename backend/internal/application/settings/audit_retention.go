package settings

import (
	"context"
	"fmt"
	"strings"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func legacyRetentionPeriod(days int) (time.Duration, error) {
	if days < 0 || days > 365 {
		return 0, fmt.Errorf("audit.retentionDays 必须在 0 到 365 之间")
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// persistedAuditRetention is shared by startup, reload and each cleanup batch.
// Explicit persisted zero overrides the file, including legacy duration values.
func persistedAuditRetention(base config.AuditConfig, value settingsdomain.AuditConfig) (config.AuditConfig, error) {
	if value.RetentionPeriod != nil {
		base.RetentionPeriod = config.Duration(*value.RetentionPeriod)
		base.RetentionSource = "runtime"
	} else if value.RetentionDays != nil {
		period, err := legacyRetentionPeriod(*value.RetentionDays)
		if err != nil {
			return base, err
		}
		base.RetentionPeriod = config.Duration(period)
		base.RetentionSource = "legacy_runtime"
	}
	return base, (auditdomain.RetentionPolicy{Period: base.RetentionPeriod.Value()}).Validate()
}

// AuditRetentionPolicy reads the authoritative store, not the asynchronously
// applied snapshot. A lost notification must not extend destructive stale work.
func (s *Service) AuditRetentionPolicy(ctx context.Context) (auditdomain.RetentionPolicy, error) {
	s.mu.RLock()
	if !s.fileCfgSet {
		s.mu.RUnlock()
		return auditdomain.RetentionPolicy{}, fmt.Errorf("audit retention requires a file configuration baseline")
	}
	base := s.fileCfg.Audit
	observed := s.revision
	s.mu.RUnlock()
	value, _, revision, found, err := s.repository.Get(ctx)
	if err != nil {
		return auditdomain.RetentionPolicy{}, err
	}
	if revision < observed {
		return auditdomain.RetentionPolicy{}, fmt.Errorf("audit retention revision regressed: persisted=%d observed=%d", revision, observed)
	}
	if found {
		base, err = persistedAuditRetention(base, value.Audit)
		if err != nil {
			return auditdomain.RetentionPolicy{}, err
		}
	}
	policy := auditdomain.RetentionPolicy{Period: base.RetentionPeriod.Value()}
	return policy, policy.Validate()
}

func mergeAuditRetention(current config.AuditConfig, input AuditConfig) (config.AuditConfig, error) {
	var period time.Duration
	var err error
	switch {
	case input.RetentionPeriodProvided:
		period, err = time.ParseDuration(strings.TrimSpace(input.RetentionPeriod))
		if err != nil {
			return current, fmt.Errorf("audit.retentionPeriod: %w", err)
		}
		if input.RetentionDaysProvided {
			legacy, legacyErr := legacyRetentionPeriod(input.RetentionDays)
			if legacyErr != nil || legacy != period {
				return current, fmt.Errorf("audit.retentionPeriod 与旧 retentionDays 冲突；请只提交 retentionPeriod")
			}
		}
	case input.RetentionDaysProvided:
		// Old clients cannot faithfully display a fractional day. Reject their
		// whole-day roundtrip instead of silently shortening existing retention.
		if current.RetentionPeriod.Value()%(24*time.Hour) != 0 {
			return current, fmt.Errorf("当前保留时长不是整天，请升级管理端并使用 audit.retentionPeriod")
		}
		period, err = legacyRetentionPeriod(input.RetentionDays)
		if err != nil {
			return current, err
		}
	default:
		return current, nil
	}
	if err := (auditdomain.RetentionPolicy{Period: period}).Validate(); err != nil {
		return current, err
	}
	current.RetentionPeriod = config.Duration(period)
	current.RetentionSource = "runtime"
	return current, nil
}

func durationPointer(value time.Duration) *time.Duration { return &value }
