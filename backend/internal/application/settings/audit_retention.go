package settings

import (
	"context"
	"fmt"
	"strings"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

func persistedAuditRetention(base settingsdomain.AuditConfig, value settingsdomain.AuditConfig) (settingsdomain.AuditConfig, error) {
	if value.RetentionPeriod != nil {
		period := *value.RetentionPeriod
		base.RetentionPeriod = &period
		base.RetentionSource = "runtime"
	} else if value.RetentionDays != nil {
		period, err := settingsdomain.LegacyRetentionPeriod(*value.RetentionDays)
		if err != nil {
			return base, err
		}
		base.RetentionPeriod = &period
		base.RetentionSource = "legacy_runtime"
	}
	return base, (auditdomain.RetentionPolicy{Period: optionalDuration(base.RetentionPeriod)}).Validate()
}

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
	policy := auditdomain.RetentionPolicy{Period: optionalDuration(base.RetentionPeriod)}
	return policy, policy.Validate()
}

func mergeAuditRetention(current *settingsdomain.AuditConfig, input AuditConfig) error {
	var period time.Duration
	var err error
	switch {
	case input.RetentionPeriodProvided:
		period, err = time.ParseDuration(strings.TrimSpace(input.RetentionPeriod))
		if err != nil {
			return fmt.Errorf("audit.retentionPeriod: %w", err)
		}
		if input.RetentionDaysProvided {
			legacy, legacyErr := settingsdomain.LegacyRetentionPeriod(input.RetentionDays)
			if legacyErr != nil || legacy != period {
				return fmt.Errorf("audit.retentionPeriod 与旧 retentionDays 冲突；请只提交 retentionPeriod")
			}
		}
	case input.RetentionDaysProvided:
		if optionalDuration(current.RetentionPeriod)%(24*time.Hour) != 0 {
			return fmt.Errorf("当前保留时长不是整天，请升级管理端并使用 audit.retentionPeriod")
		}
		period, err = settingsdomain.LegacyRetentionPeriod(input.RetentionDays)
		if err != nil {
			return err
		}
	default:
		return nil
	}
	if err := (auditdomain.RetentionPolicy{Period: period}).Validate(); err != nil {
		return err
	}
	current.RetentionPeriod = &period
	current.RetentionSource = "runtime"
	return nil
}
