package config

import (
	"fmt"
	"strings"
	"time"
)

// AccountRiskConfig controls account-level RSC risk attribution: event-driven
// checks on quality withhold, admission checks on import, and a bucketed
// patrol for clean accounts. Risky verdicts never recover and are cached
// forever, so the check load is bounded by real incidents, not pool size.
type AccountRiskConfig struct {
	RSCCheck AccountRiskRSCConfig `yaml:"rscCheck"`
}

type AccountRiskRSCConfig struct {
	Enabled bool `yaml:"enabled"`
	// Method selects the check transport: ssoProbe (default) sends one tiny
	// temporary mgw conversation with the SSO cookie and classifies by the
	// presence of the reasoning stream; homepage keeps the legacy grok.com
	// RSC payload parse (dead since grok.com stopped delivering botFlag
	// fields — kept only for rollback).
	Method      string                  `yaml:"method"`
	Concurrency int                     `yaml:"concurrency"`
	Timeout     Duration                `yaml:"timeout"`
	OnDenied    string                  `yaml:"onDenied"`
	Patrol      AccountRiskPatrolConfig `yaml:"patrol"`
	// BuildProbe enables the Build-native differential fallback for unlinked
	// Build accounts (SSO probe stays the priority whenever a Web identity is
	// linked). Defaults to off.
	BuildProbe *AccountRiskBuildProbeConfig `yaml:"buildProbe"`
}

type AccountRiskBuildProbeConfig struct {
	Enabled bool `yaml:"enabled"`
}

// BuildProbeEnabled reports the effective fallback switch (nil-safe).
func (c AccountRiskRSCConfig) BuildProbeEnabled() bool {
	return c.BuildProbe != nil && c.BuildProbe.Enabled
}

type AccountRiskPatrolConfig struct {
	Enabled    bool `yaml:"enabled"`
	BucketDays int  `yaml:"bucketDays"`
}

func DefaultAccountRiskConfig() AccountRiskConfig {
	return AccountRiskConfig{
		RSCCheck: AccountRiskRSCConfig{
			Enabled:     false,
			Method:      "ssoProbe",
			Concurrency: 2,
			Timeout:     Duration(30 * time.Second),
			OnDenied:    "flag",
			Patrol:      AccountRiskPatrolConfig{Enabled: false, BucketDays: 30},
			BuildProbe:  &AccountRiskBuildProbeConfig{Enabled: false},
		},
	}
}

func (c AccountRiskConfig) Validate() error {
	rsc := c.RSCCheck
	switch strings.TrimSpace(rsc.Method) {
	case "", "ssoProbe", "homepage":
	default:
		return fmt.Errorf("accountRisk.rscCheck.method 仅支持 ssoProbe 或 homepage")
	}
	if rsc.Concurrency != 0 && (rsc.Concurrency < 1 || rsc.Concurrency > 8) {
		return fmt.Errorf("accountRisk.rscCheck.concurrency 必须在 1 到 8 之间")
	}
	if d := rsc.Timeout.Value(); d != 0 && (d < 5*time.Second || d > 60*time.Second) {
		return fmt.Errorf("accountRisk.rscCheck.timeout 必须在 5s 到 60s 之间")
	}
	switch strings.TrimSpace(rsc.OnDenied) {
	case "", "disable", "markOnly", "flag":
	default:
		return fmt.Errorf("accountRisk.rscCheck.onDenied 仅支持 disable、markOnly 或 flag")
	}
	if rsc.Patrol.BucketDays != 0 && (rsc.Patrol.BucketDays < 7 || rsc.Patrol.BucketDays > 90) {
		return fmt.Errorf("accountRisk.rscCheck.patrol.bucketDays 必须在 7 到 90 之间")
	}
	return nil
}
