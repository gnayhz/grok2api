package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadAuditRetentionMigration(t *testing.T) {
	t.Setenv(DatabaseURLEnv, "")
	for _, tc := range []struct {
		name, yaml, source string
		period             time.Duration
		invalid            bool
	}{
		{name: "default", source: "default", period: 168 * time.Hour},
		{name: "canonical", yaml: "retentionPeriod: 36h0m0.000000001s", source: "file", period: 36*time.Hour + time.Nanosecond},
		{name: "zero", yaml: "retentionPeriod: 0", source: "file"},
		{name: "max", yaml: "retentionPeriod: 8760h", source: "file", period: 8760 * time.Hour},
		{name: "legacy days", yaml: "retentionDays: 30", source: "legacy_file", period: 720 * time.Hour},
		{name: "legacy zero", yaml: "retentionDays: 0", source: "legacy_file"},
		{name: "legacy duration", yaml: "retention: 36h", source: "legacy_file_conflict", period: 36 * time.Hour},
		{name: "legacy implicit days", yaml: "retention: 720h", source: "legacy_file_conflict", period: 168 * time.Hour},
		{name: "legacy duration zero", yaml: "retention: 0", source: "legacy_file_conflict", period: 168 * time.Hour},
		{name: "legacy both zero", yaml: "retention: 0\n  retentionDays: 0", source: "legacy_file"},
		{name: "legacy duration only enabled", yaml: "retention: 8760h\n  retentionDays: 0", source: "legacy_file_conflict", period: 8760 * time.Hour},
		{name: "legacy shorter days", yaml: "retention: 720h\n  retentionDays: 10", source: "legacy_file_conflict", period: 240 * time.Hour},
		{name: "mixed", yaml: "retentionPeriod: 168h\n  retentionDays: 7", invalid: true},
		{name: "mixed zero", yaml: "retentionPeriod: 0\n  retention: 0", invalid: true},
		{name: "mixed null", yaml: "retentionPeriod: 24h\n  retention: null", invalid: true},
		{name: "null", yaml: "retentionPeriod: null", invalid: true},
		{name: "short", yaml: "retentionPeriod: 23h", invalid: true},
		{name: "negative", yaml: "retentionPeriod: -24h", invalid: true},
		{name: "long", yaml: "retentionPeriod: 8761h", invalid: true},
		{name: "legacy short", yaml: "retention: 1h", invalid: true},
		{name: "legacy negative", yaml: "retentionDays: -1", invalid: true},
		{name: "legacy huge", yaml: "retentionDays: 9223372036854775807", invalid: true},
		{name: "unknown field", yaml: "retentionPerod: 24h", invalid: true},
		{name: "duplicate", yaml: "retentionPeriod: 24h\n  retentionPeriod: 48h", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "secrets:\n  jwtSecret: '12345678901234567890123456789012'\n  credentialEncryptionKey: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n"
			if tc.yaml != "" {
				body += "audit:\n  " + tc.yaml + "\n"
			}
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid retention accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Audit.RetentionPeriod.Value() != tc.period || cfg.Audit.RetentionSource != tc.source {
				t.Fatalf("retention = %#v", cfg.Audit)
			}
			if cfg.Audit.LegacyRetention != nil || cfg.Audit.LegacyRetentionDays != nil {
				t.Fatal("legacy knobs escaped the loader")
			}
		})
	}
}
