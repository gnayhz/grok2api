package config

import (
	"fmt"
	"net/url"
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
	// ProbeProxyURL 让 SSO 探针经代理出站（socks5/http(s)；空 = 直连）。
	// 2026-08-28 生产事故：探针从机房裸 IP 直连，首批巡检 7 连发全部被
	// 上游按降级模式服务（答案直接给、无思考头），7 个健康身份被误标
	// 风控并连坐。部署机出口不干净时应把探针指向干净代理。
	ProbeProxyURL string `yaml:"probeProxyURL"`
	// DeniedConfirmations 是 denied 定罪所需的连续确认次数（0=默认 2，
	// 范围 0-5）：未达次数的 denied 只记录不处置，待下一轮重探确认。
	DeniedConfirmations int `yaml:"deniedConfirmations"`
	// DeniedTTL 是已确认 denied verdict 的新鲜期（0=默认 24h，范围
	// 1h-720h）：过期后允许重探，误判可自愈（clean 会覆盖旧 denied）。
	DeniedTTL Duration `yaml:"deniedTTL"`
}

type AccountRiskBuildProbeConfig struct {
	Enabled bool `yaml:"enabled"`
}

// BuildProbeEnabled reports the effective fallback switch (nil-safe).
func (c AccountRiskRSCConfig) BuildProbeEnabled() bool {
	return c.BuildProbe != nil && c.BuildProbe.Enabled
}

type AccountRiskPatrolConfig struct {
	Enabled    bool     `yaml:"enabled"`
	BucketDays int      `yaml:"bucketDays"`
	Interval   Duration `yaml:"interval"`
	BatchSize  int      `yaml:"batchSize"`
}

func DefaultAccountRiskConfig() AccountRiskConfig {
	return AccountRiskConfig{
		RSCCheck: AccountRiskRSCConfig{
			Enabled:     false,
			Method:      "ssoProbe",
			Concurrency: 2,
			Timeout:     Duration(30 * time.Second),
			OnDenied:    "flag",
			Patrol:      AccountRiskPatrolConfig{Enabled: false, BucketDays: 30, Interval: Duration(15 * time.Minute), BatchSize: 50},
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
	if d := rsc.Patrol.Interval.Value(); d != 0 && (d < time.Minute || d > 6*time.Hour) {
		return fmt.Errorf("accountRisk.rscCheck.patrol.interval 必须在 1m 到 6h 之间")
	}
	if rsc.Patrol.BatchSize != 0 && (rsc.Patrol.BatchSize < 1 || rsc.Patrol.BatchSize > 200) {
		return fmt.Errorf("accountRisk.rscCheck.patrol.batchSize 必须在 1 到 200 之间")
	}
	if proxyURL := strings.TrimSpace(rsc.ProbeProxyURL); proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("accountRisk.rscCheck.probeProxyURL 必须是合法的代理 URL（http/https/socks5）")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("accountRisk.rscCheck.probeProxyURL 仅支持 http/https/socks5 代理")
		}
	}
	if rsc.DeniedConfirmations != 0 && (rsc.DeniedConfirmations < 1 || rsc.DeniedConfirmations > 5) {
		return fmt.Errorf("accountRisk.rscCheck.deniedConfirmations 必须在 1 到 5 之间（0 表示默认 2）")
	}
	if d := rsc.DeniedTTL.Value(); d != 0 && (d < time.Hour || d > 720*time.Hour) {
		return fmt.Errorf("accountRisk.rscCheck.deniedTTL 必须在 1h 到 720h 之间（0 表示默认 24h）")
	}
	return nil
}
