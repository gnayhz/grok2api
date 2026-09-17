// Package management owns quality investigation settings and their application.
package management

import (
	"encoding/json"
	"fmt"
	"time"

	qualitycourt "github.com/chenyme/grok2api/backend/internal/quality/court"
	qualityinvestigator "github.com/chenyme/grok2api/backend/internal/quality/investigator"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type Config struct {
	AccountNeedExits int `json:"account_need_exits"`
	AccountSpanNodes int `json:"account_span_nodes"`
	ExitNeedN        int `json:"exit_need_n"`
	ExitNeedK        int `json:"exit_need_k"`

	DifferentialExits int `json:"differential_exits"`
	JurorsPerExit     int `json:"jurors_per_exit"`
	ProbeBudget       int `json:"probe_budget"`

	// Duration values use Go duration strings.
	Retention            string `json:"retention"`
	EvidenceWindow       string `json:"evidence_window"`
	InvestigationTimeout string `json:"investigation_timeout"`
}

func DefaultConfig() Config {
	courtCfg := qualitycourt.DefaultConfig()
	invCfg := qualityinvestigator.DefaultConfig()
	evCfg := qualitymodel.DefaultEvidenceConfig()
	return Config{
		AccountNeedExits:     courtCfg.AccountNeedExits,
		AccountSpanNodes:     courtCfg.AccountSpanNodes,
		ExitNeedN:            courtCfg.ExitNeedN,
		ExitNeedK:            courtCfg.ExitNeedK,
		DifferentialExits:    invCfg.DifferentialExits,
		JurorsPerExit:        invCfg.JurorsPerExit,
		ProbeBudget:          invCfg.ProbeBudget,
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
	ints := []struct {
		name  string
		value int
	}{
		{"account_need_exits", input.AccountNeedExits},
		{"account_span_nodes", input.AccountSpanNodes},
		{"exit_need_n", input.ExitNeedN},
		{"exit_need_k", input.ExitNeedK},
		{"differential_exits", input.DifferentialExits},
		{"jurors_per_exit", input.JurorsPerExit},
		{"probe_budget", input.ProbeBudget},
	}
	for _, item := range ints {
		if item.value < 1 {
			return input, d, fmt.Errorf("parameter %s must be at least 1, got %d", item.name, item.value)
		}
	}
	for _, item := range ints[:4] {
		if item.value < 2 {
			return input, d, fmt.Errorf("parameter %s must be at least 2 for independent comparisons", item.name)
		}
		if item.value > 20 {
			return input, d, fmt.Errorf("parameter %s must not exceed 20", item.name)
		}
	}
	switch {
	case input.ExitNeedK > input.ExitNeedN:
		return input, d, fmt.Errorf("inconsistent thresholds: exit_need_k(%d) exceeds exit_need_n(%d)", input.ExitNeedK, input.ExitNeedN)
	case input.AccountSpanNodes > input.AccountNeedExits:
		return input, d, fmt.Errorf("inconsistent thresholds: account_span_nodes(%d) exceeds account_need_exits(%d)", input.AccountSpanNodes, input.AccountNeedExits)
	case input.JurorsPerExit < input.ExitNeedN:
		return input, d, fmt.Errorf("inconsistent probe count: jurors_per_exit(%d) is below exit_need_n(%d)", input.JurorsPerExit, input.ExitNeedN)
	case input.ProbeBudget < input.DifferentialExits+input.JurorsPerExit:
		return input, d, fmt.Errorf("inconsistent probe budget: probe_budget(%d) cannot cover differential_exits(%d)+jurors_per_exit(%d)", input.ProbeBudget, input.DifferentialExits, input.JurorsPerExit)
	}
	input.Retention = d.retention.String()
	input.EvidenceWindow = d.evidenceWindow.String()
	input.InvestigationTimeout = d.investigationTimeout.String()
	return input, d, nil
}

// persistedConfig preserves compatibility with historical missing/zero fields.
// New writes require all fields to validate and always emit schema version 2.
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
		fields := []struct {
			value    *int
			fallback int
		}{
			{&value.AccountNeedExits, defaults.AccountNeedExits}, {&value.AccountSpanNodes, defaults.AccountSpanNodes},
			{&value.ExitNeedN, defaults.ExitNeedN}, {&value.ExitNeedK, defaults.ExitNeedK},
			{&value.DifferentialExits, defaults.DifferentialExits}, {&value.JurorsPerExit, defaults.JurorsPerExit}, {&value.ProbeBudget, defaults.ProbeBudget},
		}
		for _, field := range fields {
			if *field.value == 0 {
				*field.value = field.fallback
			}
		}
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
