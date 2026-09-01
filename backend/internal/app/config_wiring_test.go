package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// TestExampleConfigQualityRetryWiring 锁定配置全链路：config.example.yaml 的
// requestRetry 段经 config.Load 与 qualityRetryRuntime 映射后，运行时结构与
// 文档值逐字段一致。此前只有子结构解码测试——字段改名/映射漂移（如漏接
// sameAccountRetry）不会被发现（测试质量审计 P2#7）。
func TestExampleConfigQualityRetryWiring(t *testing.T) {
	// The example ships placeholder secrets that Validate rejects by design;
	// swap only those two values in a temp copy so the requestRetry section
	// stays byte-identical and the full chain (parse, KnownFields, defaults,
	// env overrides, validation, runtime mapping) stays real.
	raw, err := os.ReadFile("../../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sanitized := strings.Replace(string(raw), "replace-with-at-least-32-characters", strings.Repeat("k", 48), 1)
	sanitized = strings.Replace(sanitized, "replace-with-base64-key", "hLB2nV2VqNxnDHTEkfFV1sORnGNlDYd0tJ7CWsAVSpE=", 1)
	sanitized = strings.Replace(sanitized, "replace-with-a-strong-password", "bootstrap-test-password-1234", 1)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(sanitized), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	runtime := qualityRetryRuntime(cfg.RequestRetry)

	wants := struct {
		enabled          bool
		maxAttempts      int
		onExhausted      string
		sameAccountRetry bool
		accountCooldown  time.Duration
		idleCooldown     time.Duration
		evidenceTimeout  time.Duration
		createdTimeout   time.Duration
	}{
		enabled: true, maxAttempts: 2, onExhausted: "fail_closed",
		sameAccountRetry: true, accountCooldown: 12 * time.Hour,
		idleCooldown: 15 * time.Minute, evidenceTimeout: 3500 * time.Millisecond,
		createdTimeout: 5 * time.Second,
	}
	if runtime.Enabled != wants.enabled {
		t.Errorf("Enabled = %v", runtime.Enabled)
	}
	if runtime.MaxAttempts != wants.maxAttempts {
		t.Errorf("MaxAttempts = %d", runtime.MaxAttempts)
	}
	if runtime.OnExhausted != wants.onExhausted {
		t.Errorf("OnExhausted = %q", runtime.OnExhausted)
	}
	if runtime.SameAccountRetry != wants.sameAccountRetry {
		t.Errorf("SameAccountRetry = %v", runtime.SameAccountRetry)
	}
	if runtime.AccountCooldown != wants.accountCooldown {
		t.Errorf("AccountCooldown = %s", runtime.AccountCooldown)
	}
	if runtime.IdleAccountCooldown != wants.idleCooldown {
		t.Errorf("IdleAccountCooldown = %s", runtime.IdleAccountCooldown)
	}
	if runtime.EvidenceTimeout != wants.evidenceTimeout {
		t.Errorf("EvidenceTimeout = %s", runtime.EvidenceTimeout)
	}
	if runtime.CreatedTimeout != wants.createdTimeout {
		t.Errorf("CreatedTimeout = %s", runtime.CreatedTimeout)
	}
	// example yaml is guardedModels: [] (empty = all models gated). A dropped
	// qualityRetryRuntime mapping still yields len 0, so also drive a non-empty
	// list through the same Load → mapping chain.
	if len(runtime.GuardedModels) != 0 {
		t.Errorf("example GuardedModels = %#v, want empty (all models gated)", runtime.GuardedModels)
	}

	wantModels := []string{"grok-4.5", "grok-4.6"}
	scoped := strings.Replace(sanitized, "guardedModels: []", `guardedModels: ["grok-4.5", "grok-4.6"]`, 1)
	if scoped == sanitized {
		t.Fatal("example yaml missing guardedModels: []")
	}
	scopedPath := filepath.Join(t.TempDir(), "config-scoped.yaml")
	if err := os.WriteFile(scopedPath, []byte(scoped), 0o600); err != nil {
		t.Fatal(err)
	}
	scopedCfg, err := config.Load(scopedPath)
	if err != nil {
		t.Fatalf("load scoped example: %v", err)
	}
	scopedRuntime := qualityRetryRuntime(scopedCfg.RequestRetry)
	if len(scopedRuntime.GuardedModels) != 2 || scopedRuntime.GuardedModels[0] != wantModels[0] || scopedRuntime.GuardedModels[1] != wantModels[1] {
		t.Errorf("GuardedModels mapping = %#v, want %#v", scopedRuntime.GuardedModels, wantModels)
	}
}
