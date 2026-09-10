package config

import (
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"time"
)

// EgressConfig groups exit-IP rotation settings. 旧出口质量守卫配置节
// (隔离/跨账号确认/软冷却/试探期)已随旧质量链删除(切换手册第2步)。
type EgressConfig struct {
	Runtime  EgressRuntimeConfig  `yaml:"runtime"`
	Rotation EgressRotationConfig `yaml:"rotation"`
}

// EgressRuntimeConfig is process-local startup policy, outside live routing settings.
type EgressRuntimeConfig struct {
	netbudget.Limits `yaml:",inline"`
	QueueTimeout     Duration `yaml:"queueTimeout"`
}

func (c EgressRuntimeConfig) LimitsValue() netbudget.Limits {
	l := c.Limits
	l.QueueTimeout = c.QueueTimeout.Value()
	return l.Defaults()
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
}

// Validate 拒绝明显越界的取值。零值表示"使用默认", 不在拒绝之列; 此前 egress
// 段完全没有校验, 写错单位(如 24 被当成 24ns)会被运行时归一化静默吞掉, 排障
// 无从下手——与其他配置段"校验失败拒绝启动"的严格风格对齐。
func (c EgressConfig) Validate() error {
	for _, n := range []int{c.Runtime.Connections, c.Runtime.Dialing, c.Runtime.Requests, c.Runtime.Waiters, c.Runtime.Clients} {
		if n < 0 || n > 65536 {
			return errors.New("egress.runtime resource limits must be between 0 and 65536")
		}
	}
	if d := c.Runtime.QueueTimeout.Value(); d != 0 && (d < time.Millisecond || d > time.Minute) {
		return errors.New("egress.runtime.queueTimeout must be between 1ms and 1m")
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
	return nil
}

// DefaultEgressConfig returns the recommended defaults.
func DefaultEgressConfig() EgressConfig {
	return EgressConfig{
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
		},
	}
}
