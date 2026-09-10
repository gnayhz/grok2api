package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestDefaultRequestRetryBudgetDefaults 锁定全局请求预算默认值（蓝图 §3.2）：
// MaxAttempts=2（1 次初始 + 1 次换号重试）、EvidenceTimeout=3.5s（防死锁
// 兜底，降智流已被零延迟拦截截胡）、CreatedTimeout=5s（首事件截止）。
func TestDefaultRequestRetryBudgetDefaults(t *testing.T) {
	cfg := defaultConfig()
	if got := cfg.RequestRetry.MaxAttempts; got != 2 {
		t.Fatalf("MaxAttempts default = %d, want 2", got)
	}
	if got := cfg.RequestRetry.EvidenceTimeout.Value(); got != 3500*time.Millisecond {
		t.Fatalf("EvidenceTimeout default = %v, want 3.5s", got)
	}
	if got := cfg.RequestRetry.CreatedTimeout.Value(); got != 5*time.Second {
		t.Fatalf("CreatedTimeout default = %v, want 5s", got)
	}
	if cfg.RequestRetry.OnExhausted != "fail_closed" {
		t.Fatalf("OnExhausted default = %q, want fail_closed", cfg.RequestRetry.OnExhausted)
	}
	if len(cfg.RequestRetry.GuardedModels) != 0 {
		t.Fatalf("GuardedModels default = %#v, want empty (bootstrap domain defaults)", cfg.RequestRetry.GuardedModels)
	}
}

// The file and management surfaces share the domain budget, while request
// execution separately enforces total attempts and admission deadlines.
func TestRequestRetryBudgetCap(t *testing.T) {
	base := defaultConfig()
	base.Secrets.JWTSecret = strings.Repeat("k", 32)
	base.Secrets.CredentialEncryptionKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for _, enabled := range []bool{true, false} {
		base.RequestRetry.Enabled = enabled
		for _, value := range []int{0, 1, 4, 6, 100} {
			base.RequestRetry.MaxAttempts = value
			if err := base.Validate(); err != nil {
				t.Fatalf("maxAttempts=%d enabled=%v: %v", value, enabled, err)
			}
		}
		for _, value := range []int{-1, 101} {
			base.RequestRetry.MaxAttempts = value
			if err := base.Validate(); err == nil {
				t.Fatalf("invalid budget %d enabled=%v accepted", value, enabled)
			}
		}
	}
}

// TestUnmarshalRequestRetryFields 显式 YAML 覆盖默认值。
func TestUnmarshalRequestRetryFields(t *testing.T) {
	var section RequestRetryConfig
	nl := string(rune(10))
	yamlText := "enabled: true" + nl + nl + "evidenceTimeout: 4s" + nl + "maxAttempts: 2" + nl + "createdTimeout: 8s" + nl + "onExhausted: fail_closed" + nl + "accountCooldown: 12h" + nl + "guardedModels: [\"grok-4.5\", \"grok-4.6\"]" + nl
	if err := yaml.NewDecoder(bytes.NewReader([]byte(yamlText))).Decode(&section); err != nil {
		t.Fatal(err)
	}
	if section.EvidenceTimeout.Value() != 4*time.Second {
		t.Fatalf("evidenceTimeout = %s, want 4s", section.EvidenceTimeout.Value())
	}
	if section.CreatedTimeout.Value() != 8*time.Second {
		t.Fatalf("createdTimeout = %s, want 8s", section.CreatedTimeout.Value())
	}
	wantModels := []string{"grok-4.5", "grok-4.6"}
	if len(section.GuardedModels) != 2 || section.GuardedModels[0] != wantModels[0] || section.GuardedModels[1] != wantModels[1] {
		t.Fatalf("guardedModels = %#v, want %#v", section.GuardedModels, wantModels)
	}
}

// TestLegacyQualityGuardKeyRejected 旧 sidecar 品牌键 qualityGuard 已随功能一起
// 移除；KnownFields 必须显式拒绝而不是静默忽略，防止残留配置悄悄复活死品牌。
func TestLegacyQualityGuardKeyRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	nl := string(rune(10))
	content := "requestRetry:" + nl + "  enabled: true" + nl +
		"qualityGuard:" + nl + "  requestRetry:" + nl + "    enabled: true" + nl
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "qualityGuard") {
		t.Fatalf("legacy qualityGuard key must be rejected with a clear error, got: %v", err)
	}
}

// TestRemovedRequestRetryKeysRejected 逐一锁定已删除的历史键被加载器按名
// 拒绝（KnownFields 严格解码在 Load 路径）——既防止残留配置复活死机制，
// 也是未来删除任何键时的回归模板（把新键名加进列表即可）。
func TestRemovedRequestRetryKeysRejected(t *testing.T) {
	t.Parallel()
	nl := string(rune(10))
	for _, key := range []string{"holdTimeout", "minOutputTokens", "earlyHeaderAbort", "terminalBurstThreshold"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		content := "requestRetry:" + nl + "  enabled: true" + nl + "  " + key + ": 1s" + nl
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("removed key %q must be rejected by name, got: %v", key, err)
		}
	}
}

func TestValidateRequestRetryIdleAccountCooldown(t *testing.T) {
	t.Parallel()
	base := func(d time.Duration) RequestRetryConfig {
		return RequestRetryConfig{Enabled: true, IdleAccountCooldown: Duration(d)}
	}
	for _, invalid := range []time.Duration{-time.Nanosecond, 169 * time.Hour} {
		if err := validateRequestRetry(base(invalid)); err == nil || !strings.Contains(err.Error(), "idleAccountCooldown") {
			t.Fatalf("idle cooldown %v should be rejected, got %v", invalid, err)
		}
	}
	for _, valid := range []time.Duration{0, time.Millisecond, 59 * time.Second, time.Minute, 24 * time.Hour, 168 * time.Hour} {
		if err := validateRequestRetry(base(valid)); err != nil {
			t.Fatalf("idle cooldown %v should be accepted, got %v", valid, err)
		}
	}
}
