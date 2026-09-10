package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
	qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"
)

func loadGuardPolicyFile(t *testing.T, fields string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "secrets:\n  jwtSecret: '12345678901234567890123456789012'\n  credentialEncryptionKey: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\nrequestRetry:\n" + fields
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
}

func TestGuardFileInvalidPolicyCannotBecomeReady(t *testing.T) {
	for _, fields := range []string{
		"  enabled: true\n  guardedModels: ['typo_provider:grok-4.6']\n",
		"  enabled: false\n  evidenceTimeout: -1s\n",
		"  enabled: false\n  maxAttempts: -1\n",
		"  enabled: true\n  admissionTimeout: -1s\n",
		"  enabled: false\n  toolAdmissionTimeout: 25h\n",
	} {
		t.Run(fields, func(t *testing.T) {
			cfg, err := loadGuardPolicyFile(t, fields)
			if err != nil {
				return
			}
			cfg.BootstrapAdmin.Username, cfg.BootstrapAdmin.Password = "test-admin", "fixture-admin-password"
			disabled := false
			cfg.Server.UpdateCheckEnabled = &disabled
			a, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = a.Close() })
			snap, err := a.qualityGuard.Snapshot()
			if err == nil {
				t.Fatalf("invalid file policy became ready: enabled=%v models=%v evidence=%s jurisdiction=%v", snap.Enabled, snap.GuardedModels, snap.EvidenceTimeout, snap.Jurisdiction("grok_build", "grok-4.6"))
			}
		})
	}
}

func TestGuardFileAndManagementAcceptSamePolicy(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		apply      func(*qualityguard.Config)
	}{
		{"attempts", "  maxAttempts: 6\n", func(c *qualityguard.Config) { c.MaxAttempts = 6 }},
		{"evidence", "  evidenceTimeout: 10m\n", func(c *qualityguard.Config) { c.EvidenceTimeout = 10 * time.Minute }},
		{"created", "  createdTimeout: 700ms\n", func(c *qualityguard.Config) { c.CreatedTimeout = 700 * time.Millisecond }},
		{"admission", "  admissionTimeout: 12m\n", func(c *qualityguard.Config) { c.AdmissionTimeout = 12 * time.Minute }},
		{"tool", "  toolAdmissionTimeout: 20m\n", func(c *qualityguard.Config) { c.ToolAdmissionTimeout = 20 * time.Minute }},
		{"cooldown", "  accountCooldown: 30s\n", func(c *qualityguard.Config) { c.AccountCooldown = 30 * time.Second }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := qualityguard.New(qualityguard.DefaultConfig(), nil)
			value := s.Config()
			test.apply(&value)
			accepted, err := s.Update(context.Background(), value)
			if err != nil {
				t.Fatalf("existing management policy invalid: %v", err)
			}
			file, err := loadGuardPolicyFile(t, "  enabled: true\n"+test.yaml)
			if err != nil {
				t.Fatalf("same accepted policy cannot load from file: %v", err)
			}
			actual := qualityguard.New(qualityGuardConfig(file.RequestRetry), nil).Config()
			accepted.Revision = 0
			if !reflect.DeepEqual(actual, accepted) {
				t.Fatalf("file differs: %+v / %+v", actual, accepted)
			}
		})
	}
}

func TestGuardFileBootstrapCompatibilityThroughApplication(t *testing.T) {
	cfg, err := loadGuardPolicyFile(t, "  enabled: true\n  onExhausted: fail_open\n  guardedModels: []\n  maxAttempts: 0\n  admissionTimeout: 40s\n  toolAdmissionTimeout: 4m\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg.BootstrapAdmin.Username, cfg.BootstrapAdmin.Password = "test-admin", "fixture-admin-password"
	disabled := false
	cfg.Server.UpdateCheckEnabled = &disabled
	a, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	snap, err := a.qualityGuard.Snapshot()
	if err != nil || !snap.Enabled || snap.MaxAttempts != 2 || !snap.Jurisdiction("grok_build", "grok-4.6") || snap.ExhaustionPolicy() != "fail_closed" || snap.AdmissionTimeout != 40*time.Second || snap.ToolAdmissionTimeout != 4*time.Minute {
		t.Fatalf("bootstrap policy=%+v err=%v", snap, err)
	}
	current := a.qualityGuard.Config()
	current.Enabled, current.GuardedModels = false, nil
	saved, err := a.qualityGuard.Update(context.Background(), current)
	if err != nil || saved.Enabled || len(saved.GuardedModels) != 0 {
		t.Fatalf("explicit management disable changed: %+v %v", saved, err)
	}
	reset, err := a.qualityGuard.ResetToDefaults(context.Background(), saved.Revision)
	if err != nil || reset.Revision != 2 || !reset.Enabled || reset.AdmissionTimeout != 40*time.Second || reset.ToolAdmissionTimeout != 4*time.Minute || !reset.Jurisdiction("grok_build", "grok-4.6") {
		t.Fatalf("reset=%+v %v", reset, err)
	}
	frozen := (qualityGuardSnapshotSource{service: a.qualityGuard}).GuardSnapshot()
	changed := reset
	changed.AdmissionTimeout = time.Minute
	if _, err := a.qualityGuard.Update(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if frozen.Err != nil || frozen.Runtime.Revision != 2 || frozen.Runtime.AdmissionTimeout != 40*time.Second {
		t.Fatalf("old snapshot changed: %+v", frozen)
	}
}
