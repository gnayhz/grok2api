package config

import (
	"fmt"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"gopkg.in/yaml.v3"
)

// resolveAuditRetention runs after the strict YAML decoder. Legacy fields are
// read only migration inputs and are erased before configuration leaves Load.
func resolveAuditRetention(cfg *AuditConfig, data []byte) error {
	var presence struct {
		Audit map[string]yaml.Node `yaml:"audit"`
	}
	if err := yaml.Unmarshal(data, &presence); err != nil {
		return err
	}
	_, durationPresent := presence.Audit["retention"]
	_, daysPresent := presence.Audit["retentionDays"]
	legacy := durationPresent || daysPresent
	if node, present := presence.Audit["retentionPeriod"]; present {
		if node.Tag == "!!null" {
			return fmt.Errorf("audit.retentionPeriod 不能为 null；永久保留请显式使用 0")
		}
		if legacy {
			return fmt.Errorf("audit.retentionPeriod 不能与旧 retention/retentionDays 混写，请迁移为一个保留时长")
		}
		cfg.RetentionSource = "file"
		return nil
	}
	if !legacy {
		return nil
	}
	daysPeriod := auditdomain.DefaultRetentionPeriod
	if cfg.LegacyRetentionDays != nil {
		days := *cfg.LegacyRetentionDays
		if days < 0 || days > 365 {
			return fmt.Errorf("旧 audit.retentionDays 必须在 0 到 365 之间")
		}
		daysPeriod = time.Duration(days) * 24 * time.Hour
	}
	var durationPeriod time.Duration
	if cfg.LegacyRetention != nil {
		durationPeriod = cfg.LegacyRetention.Value()
		if err := (auditdomain.RetentionPolicy{Period: durationPeriod}).Validate(); err != nil {
			return fmt.Errorf("旧 audit.retention: %w", err)
		}
	}
	period := daysPeriod
	if durationPeriod > 0 && (period == 0 || durationPeriod < period) {
		period = durationPeriod
	}
	cfg.RetentionPeriod = Duration(period)
	cfg.RetentionSource = "legacy_file"
	if cfg.LegacyRetention != nil && durationPeriod != daysPeriod {
		cfg.RetentionSource = "legacy_file_conflict"
	}
	cfg.LegacyRetentionDays = nil
	cfg.LegacyRetention = nil
	return nil
}
