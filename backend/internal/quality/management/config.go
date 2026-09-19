// Package management owns quality investigation settings and their application.
package management

import (
	"encoding/json"
	"fmt"
	"time"

	qualitycourt "github.com/chenyme/grok2api/backend/internal/quality/court"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type Config struct {
	// Duration values use Go duration strings.
	Retention            string `json:"retention"`
	EvidenceWindow       string `json:"evidence_window"`
	InvestigationTimeout string `json:"investigation_timeout"`
}

func DefaultConfig() Config {
	courtCfg := qualitycourt.DefaultConfig()
	evCfg := qualitymodel.DefaultEvidenceConfig()
	return Config{
		Retention:            evCfg.Retention.String(),
		EvidenceWindow:       evCfg.Window.String(),
		InvestigationTimeout: courtCfg.InvestigationTimeout.String(),
	}
}

type tunableDurations struct {
	retention            time.Duration
	evidenceWindow       time.Duration
	investigationTimeout time.Duration
}

func normalize(input Config) (Config, tunableDurations, error) {
	var d tunableDurations
	var err error
	parse := func(name, value string, into *time.Duration) {
		if err != nil {
			return
		}
		parsed, parseErr := time.ParseDuration(value)
		if parseErr != nil || parsed <= 0 {
			err = fmt.Errorf("parameter %s is invalid: %q", name, value)
			return
		}
		*into = parsed
	}
	parse("retention", input.Retention, &d.retention)
	parse("evidence_window", input.EvidenceWindow, &d.evidenceWindow)
	parse("investigation_timeout", input.InvestigationTimeout, &d.investigationTimeout)
	if err != nil {
		return input, d, err
	}
	input.Retention = d.retention.String()
	input.EvidenceWindow = d.evidenceWindow.String()
	input.InvestigationTimeout = d.investigationTimeout.String()
	return input, d, nil
}

// Persisted settings contain only lifecycle bounds. Retired decision thresholds
// are ignored on read and never emitted in a new settings revision.
type persistedConfig struct {
	Config
	SchemaVersion           int  `json:"_schema_version"`
	NetworkCapacityMigrated bool `json:"_network_capacity_migrated"`
}

func decodePersisted(payload []byte) (Config, error) {
	value := persistedConfig{Config: DefaultConfig()}
	if err := json.Unmarshal(payload, &value); err != nil {
		return Config{}, err
	}
	if value.SchemaVersion > repository.QualitySettingsSchemaVersion {
		return Config{}, fmt.Errorf("unsupported quality settings schema %d", value.SchemaVersion)
	}
	if value.SchemaVersion < repository.QualitySettingsSchemaVersion {
		defaults := DefaultConfig()
		if value.Retention == "" {
			value.Retention = defaults.Retention
		}
		if value.EvidenceWindow == "" {
			value.EvidenceWindow = defaults.EvidenceWindow
		}
		if value.InvestigationTimeout == "" {
			value.InvestigationTimeout = defaults.InvestigationTimeout
		}
	}
	normalized, _, err := normalize(value.Config)
	return normalized, err
}
