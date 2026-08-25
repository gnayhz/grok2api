package config

import (
	"errors"
	"time"
)

// EgressConfig groups exit-IP quality guard and rotation settings.
type EgressConfig struct {
	QualityGuard EgressQualityGuardConfig `yaml:"qualityGuard"`
	Rotation     EgressRotationConfig     `yaml:"rotation"`
}

// EgressQualityGuardConfig controls quarantine of fixed egress nodes whose
// exit IP routes requests to the degraded model.
type EgressQualityGuardConfig struct {
	// QuarantineCooldown keeps a degraded node out of rotation while its exit
	// IP is rotated and verified (default 24h; successful verification
	// releases early).
	QuarantineCooldown Duration `yaml:"quarantineCooldown"`
	// CrossAccountThreshold: distinct accounts degrading on the same node
	// inside CrossAccountWindow confirm an exit-IP problem without RSC
	// attribution. Values below 2 disable the fallback (default 2).
	CrossAccountThreshold int `yaml:"crossAccountThreshold"`
	// CrossAccountWindow bounds how long degrade evidence stays decisive
	// (default 30m).
	CrossAccountWindow Duration `yaml:"crossAccountWindow"`
	// TentativeReleaseCooldown applies when rotation finished but canary
	// verification was inconclusive (default 30m).
	TentativeReleaseCooldown Duration `yaml:"tentativeReleaseCooldown"`
	// SoftCooldownBase 是降智证据软冷却的起步时长:证据出现即让全池账号避开
	// 该出口,不等归因;重复证据指数翻倍,封顶 SoftCooldownMax(默认 5m/1h)。
	SoftCooldownBase Duration `yaml:"softCooldownBase"`
	SoftCooldownMax  Duration `yaml:"softCooldownMax"`
}

// EgressRotationConfig controls automatic exit-IP rotation webhooks.
type EgressRotationConfig struct {
	Enabled                  bool     `yaml:"enabled"`
	MaxAttemptsPerQuarantine int      `yaml:"maxAttemptsPerQuarantine"`
	MinNodeInterval          Duration `yaml:"minNodeInterval"`
	MaxGlobalPerHour         int      `yaml:"maxGlobalPerHour"`
	WebhookTimeout           Duration `yaml:"webhookTimeout"`
	WebhookRetries           int      `yaml:"webhookRetries"`
	SettleDelay              Duration `yaml:"settleDelay"`
	ProbeTimeout             Duration `yaml:"probeTimeout"`
	ProbeInterval            Duration `yaml:"probeInterval"`
	// CanaryModelPublicID selects the model route for the one-shot
	// verification request after rotation. Empty disables verification
	// (tentative re-admission with a short cooldown instead).
	CanaryModelPublicID  string   `yaml:"canaryModelPublicId"`
	CanaryCreatedTimeout Duration `yaml:"canaryCreatedTimeout"`
}

// Validate 拒绝明显越界的取值。零值表示"使用默认", 不在拒绝之列; 此前 egress
// 段完全没有校验, 写错单位(如 24 被当成 24ns)会被运行时归一化静默吞掉, 排障
// 无从下手——与其他配置段"校验失败拒绝启动"的严格风格对齐。
func (c EgressConfig) Validate() error {
	q := c.QualityGuard
	if v := q.QuarantineCooldown.Value(); v != 0 && (v < time.Minute || v > 720*time.Hour) {
		return errors.New("egress.qualityGuard.quarantineCooldown 必须在 1 分钟到 720 小时之间（0=默认 24h）")
	}
	if q.CrossAccountThreshold < 0 || q.CrossAccountThreshold > 100 {
		return errors.New("egress.qualityGuard.crossAccountThreshold 必须在 0 到 100 之间（<2 关闭跨账号兜底）")
	}
	if v := q.CrossAccountWindow.Value(); v != 0 && (v < time.Minute || v > 24*time.Hour) {
		return errors.New("egress.qualityGuard.crossAccountWindow 必须在 1 分钟到 24 小时之间（0=默认 30m）")
	}
	if v := q.TentativeReleaseCooldown.Value(); v != 0 && (v < time.Minute || v > 24*time.Hour) {
		return errors.New("egress.qualityGuard.tentativeReleaseCooldown 必须在 1 分钟到 24 小时之间（0=默认 30m）")
	}
	if base, max := q.SoftCooldownBase.Value(), q.SoftCooldownMax.Value(); base != 0 || max != 0 {
		if base != 0 && (base < time.Second || base > 24*time.Hour) {
			return errors.New("egress.qualityGuard.softCooldownBase 必须在 1 秒到 24 小时之间（0=默认 5m）")
		}
		if max != 0 && (max < time.Second || max > 720*time.Hour) {
			return errors.New("egress.qualityGuard.softCooldownMax 必须在 1 秒到 720 小时之间（0=默认 1h）")
		}
		if base != 0 && max != 0 && max < base {
			return errors.New("egress.qualityGuard.softCooldownMax 不能小于 softCooldownBase")
		}
	}
	rot := c.Rotation
	if rot.MaxAttemptsPerQuarantine < 0 || rot.MaxAttemptsPerQuarantine > 100 {
		return errors.New("egress.rotation.maxAttemptsPerQuarantine 必须在 0 到 100 之间（0=默认 3）")
	}
	// minNodeInterval 下限放到 10s:该值纯配置驱动("配置多久就是多久"),
	// 防重启风暴由 maxGlobalPerHour(全局每小时上限)兜底,分钟级下限只会
	// 阻止运维按需配置亚分钟间隔。
	if v := rot.MinNodeInterval.Value(); v != 0 && (v < 10*time.Second || v > 24*time.Hour) {
		return errors.New("egress.rotation.minNodeInterval 必须在 10 秒到 24 小时之间（0=默认 3m）")
	}
	if rot.MaxGlobalPerHour < 0 || rot.MaxGlobalPerHour > 10000 {
		return errors.New("egress.rotation.maxGlobalPerHour 必须在 0 到 10000 之间（0=默认 6）")
	}
	if v := rot.WebhookTimeout.Value(); v != 0 && (v < time.Second || v > 10*time.Minute) {
		return errors.New("egress.rotation.webhookTimeout 必须在 1 秒到 10 分钟之间（0=默认 15s）")
	}
	if rot.WebhookRetries < 0 || rot.WebhookRetries > 10 {
		return errors.New("egress.rotation.webhookRetries 必须在 0 到 10 之间")
	}
	if v := rot.SettleDelay.Value(); v != 0 && (v < time.Second || v > 10*time.Minute) {
		return errors.New("egress.rotation.settleDelay 必须在 1 秒到 10 分钟之间（0=默认 20s）")
	}
	if v := rot.ProbeTimeout.Value(); v != 0 && (v < time.Second || v > 10*time.Minute) {
		return errors.New("egress.rotation.probeTimeout 必须在 1 秒到 10 分钟之间（0=默认 2m）")
	}
	if v := rot.ProbeInterval.Value(); v != 0 && (v < time.Second || v > time.Hour) {
		return errors.New("egress.rotation.probeInterval 必须在 1 秒到 1 小时之间（0=默认 5s）")
	}
	if v := rot.CanaryCreatedTimeout.Value(); v != 0 && (v < time.Second || v > 5*time.Minute) {
		return errors.New("egress.rotation.canaryCreatedTimeout 必须在 1 秒到 5 分钟之间（0=默认 10s）")
	}
	if len(rot.CanaryModelPublicID) > 128 {
		return errors.New("egress.rotation.canaryModelPublicId 过长（≤128 字符）")
	}
	return nil
}

// DefaultEgressConfig returns the recommended defaults.
func DefaultEgressConfig() EgressConfig {
	return EgressConfig{
		QualityGuard: EgressQualityGuardConfig{
			QuarantineCooldown:       Duration(24 * time.Hour),
			CrossAccountThreshold:    2,
			CrossAccountWindow:       Duration(30 * time.Minute),
			TentativeReleaseCooldown: Duration(30 * time.Minute),
			SoftCooldownBase:         Duration(5 * time.Minute),
			SoftCooldownMax:          Duration(time.Hour),
		},
		Rotation: EgressRotationConfig{
			Enabled:                  true,
			MaxAttemptsPerQuarantine: 3,
			MinNodeInterval:          Duration(3 * time.Minute),
			MaxGlobalPerHour:         6,
			WebhookTimeout:           Duration(15 * time.Second),
			WebhookRetries:           2,
			SettleDelay:              Duration(20 * time.Second),
			ProbeTimeout:             Duration(2 * time.Minute),
			ProbeInterval:            Duration(5 * time.Second),
			CanaryCreatedTimeout:     Duration(10 * time.Second),
		},
	}
}
