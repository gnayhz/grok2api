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

// TestValidateRequestRetryEarlyHeaderAbort 边界表（D 审查缺口）。
func TestValidateRequestRetryEarlyHeaderAbort(t *testing.T) {
	base := func(d time.Duration) RequestRetryConfig {
		return RequestRetryConfig{Enabled: true, EarlyHeaderAbort: Duration(d)}
	}
	cases := []struct {
		name    string
		value   time.Duration
		wantErr bool
	}{
		{"zero off", 0, false},
		{"min 3s", 3 * time.Second, false},
		{"max 60s", 60 * time.Second, false},
		{"below min", 2999 * time.Millisecond, true},
		{"above max", 61 * time.Second, true},
	}
	for _, tc := range cases {
		err := validateRequestRetry(base(tc.value))
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
	if err := validateRequestRetry(RequestRetryConfig{Enabled: false, EarlyHeaderAbort: Duration(time.Second)}); err != nil {
		t.Fatalf("disabled config must skip validation: %v", err)
	}
}

// TestDefaultRequestRetrySameAccountRetryTrue 默认值与注释/示例一致（true）。
func TestDefaultRequestRetrySameAccountRetryTrue(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.RequestRetry.SameAccountRetry {
		t.Fatal("SameAccountRetry default must stay true (comment + example document it)")
	}
}

// TestUnmarshalRequestRetryFields 显式 YAML 覆盖默认值。
func TestUnmarshalRequestRetryFields(t *testing.T) {
	var section RequestRetryConfig
	nl := string(rune(10))
	yamlText := "enabled: true" + nl + "sameAccountRetry: false" + nl + "earlyHeaderAbort: 10s" + nl + "maxAttempts: 4" + nl + "holdTimeout: 2s" + nl + "onExhausted: fail_closed" + nl + "accountCooldown: 12h" + nl
	if err := yaml.NewDecoder(bytes.NewReader([]byte(yamlText))).Decode(&section); err != nil {
		t.Fatal(err)
	}
	if section.SameAccountRetry {
		t.Fatal("explicit sameAccountRetry:false must load as false")
	}
	if section.EarlyHeaderAbort.Value() != 10*time.Second {
		t.Fatalf("earlyHeaderAbort = %s, want 10s", section.EarlyHeaderAbort.Value())
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
