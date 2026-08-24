package config

import (
	"strings"
	"testing"
	"time"
)

// egress 段边界校验:此前完全没有校验, 写错单位/越界的取值被运行时归一化
// 静默吞掉。现在与其他配置段一致——校验失败拒绝启动。
func TestEgressConfigValidateBounds(t *testing.T) {
	valid := DefaultEgressConfig()
	if err := valid.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	// 零值=使用默认, 允许。
	if err := (EgressConfig{}).Validate(); err != nil {
		t.Fatalf("zero config must validate: %v", err)
	}

	cases := []struct {
		name   string
		mut    func(*EgressConfig)
		substr string
	}{
		{"quarantineCooldown 过小", func(c *EgressConfig) { c.QualityGuard.QuarantineCooldown = Duration(24 * time.Nanosecond) }, "quarantineCooldown"},
		{"quarantineCooldown 过大", func(c *EgressConfig) { c.QualityGuard.QuarantineCooldown = Duration(1000 * time.Hour) }, "quarantineCooldown"},
		{"crossAccountThreshold 负数", func(c *EgressConfig) { c.QualityGuard.CrossAccountThreshold = -1 }, "crossAccountThreshold"},
		{"softCooldownMax 小于 base", func(c *EgressConfig) {
			c.QualityGuard.SoftCooldownBase = Duration(time.Hour)
			c.QualityGuard.SoftCooldownMax = Duration(time.Minute)
		}, "softCooldownMax"},
		{"maxAttempts 过大", func(c *EgressConfig) { c.Rotation.MaxAttemptsPerQuarantine = 101 }, "maxAttemptsPerQuarantine"},
		{"webhookTimeout 过小", func(c *EgressConfig) { c.Rotation.WebhookTimeout = Duration(time.Nanosecond) }, "webhookTimeout"},
		{"canaryModelPublicID 过长", func(c *EgressConfig) { c.Rotation.CanaryModelPublicID = strings.Repeat("m", 129) }, "canaryModelPublicId"},
	}
	for _, testCase := range cases {
		value := DefaultEgressConfig()
		testCase.mut(&value)
		err := value.Validate()
		if err == nil || !strings.Contains(err.Error(), testCase.substr) {
			t.Fatalf("%s: err = %v, want containing %q", testCase.name, err, testCase.substr)
		}
	}
}
