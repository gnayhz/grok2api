package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func saveAuditRetention(t *testing.T, service *settingsapp.Service, period string) settingsapp.Snapshot {
	t.Helper()
	before := service.Get()
	input := before.Config
	input.Audit.RetentionPeriod, input.Audit.RetentionPeriodProvided = period, true
	input.Audit.RetentionDaysProvided = false
	saved, err := service.Update(context.Background(), before.Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestAuditRetentionSettingsMigrationIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			dbA, dbB := settingsDatabasePair(t, dialect)
			base := settingsFileBaseline(t)
			base.Audit.RetentionPeriod, base.Audit.RetentionSource = config.Duration(8760*time.Hour), "legacy_file_conflict"
			a := settingsServiceOn(t, dbA, base, nil, nil)
			saveAuditRetention(t, a, "48h")
			cipher, err := security.NewCipher(base.Secrets.CredentialEncryptionKey)
			if err != nil {
				t.Fatal(err)
			}
			repo := NewRuntimeSettingsRepository(dbA, cipher)
			value, _, revision, _, err := repo.Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Seed the actual persisted legacy format. All unrelated settings stay
			// valid so this exercises the real startup merger and SQL decoder.
			for _, days := range []int{0, 2} {
				value.Audit.RetentionPeriod, value.Audit.RetentionDays = nil, &days
				_, revision, err = repo.Save(ctx, value, revision)
				if err != nil {
					t.Fatal(err)
				}
				restarted := settingsServiceOn(t, dbB, base, nil, nil)
				policy, err := restarted.AuditRetentionPolicy(ctx)
				if err != nil || policy.Period != time.Duration(days)*24*time.Hour || restarted.Get().Config.Audit.RetentionSource != "legacy_runtime" {
					t.Fatalf("legacy policy=%+v err=%v", policy, err)
				}
			}
			value.Audit.RetentionDays = nil
			_, revision, err = repo.Save(ctx, value, revision)
			if err != nil {
				t.Fatal(err)
			}
			restarted := settingsServiceOn(t, dbB, base, nil, nil)
			if restarted.Get().Config.Audit.RetentionPeriod != "8760h" {
				t.Fatal("unrelated legacy override lost file retention")
			}
			saved := saveAuditRetention(t, restarted, "36h0m0.000000001s")
			var row runtimeSettingsModel
			if err := dbA.db.Where("key = ?", runtimeSettingsKey).First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if strings.Contains(row.ValueJSON, "RetentionDays") || !strings.Contains(row.ValueJSON, "RetentionPeriod") {
				t.Fatal("save retained a second retention knob")
			}
			again := settingsServiceOn(t, dbA, base, nil, nil)
			if again.Get().Config.Audit.RetentionPeriod != saved.Config.Audit.RetentionPeriod {
				t.Fatal("restart rounded retention")
			}
			legacy := again.Get().Config
			legacy.Audit.RetentionPeriodProvided = false
			legacy.Audit.RetentionDaysProvided, legacy.Audit.RetentionDays = true, 1
			if _, err := again.Update(ctx, again.Get().Revision, legacy); !errors.Is(err, settingsapp.ErrInvalidInput) {
				t.Fatalf("fractional legacy writer: %v", err)
			}
			if _, err := a.Update(ctx, a.Get().Revision, a.Get().Config); !errors.Is(err, settingsapp.ErrConflict) {
				t.Fatalf("stale writer: %v", err)
			}
			reset, err := again.ResetToDefaults(ctx, again.Get().Revision)
			if err != nil || reset.Revision <= saved.Revision || reset.Config.Audit.RetentionPeriod != "8760h" || reset.Config.Audit.FileRetentionSource != "legacy_file_conflict" {
				t.Fatalf("reset=%+v err=%v", reset.Config.Audit, err)
			}
			// A peer that never reloaded observes reset directly at its next batch.
			policy, err := restarted.AuditRetentionPolicy(ctx)
			if err != nil || policy.Period != 8760*time.Hour {
				t.Fatalf("reset source=%+v err=%v", policy, err)
			}
			if fresh := settingsServiceOn(t, dbB, base, nil, nil); fresh.Get().Revision != reset.Revision || fresh.Get().Config.Audit.RetentionPeriod != "8760h" {
				t.Fatal("restart lost reset retention baseline")
			}
			canonical, invalidDays := 48*time.Hour, -1
			value.Audit.RetentionPeriod, value.Audit.RetentionDays = &canonical, &invalidDays
			_, revision, err = repo.Save(ctx, value, reset.Revision)
			if err != nil {
				t.Fatal(err)
			}
			policy, err = restarted.AuditRetentionPolicy(ctx)
			if err != nil || policy.Period != canonical {
				t.Fatalf("canonical precedence=%+v err=%v", policy, err)
			}
			value.Audit.RetentionPeriod = nil
			if _, _, err := repo.Save(ctx, value, revision); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.AuditRetentionPolicy(ctx); err == nil {
				t.Fatal("invalid persisted policy used cached fallback")
			}
			if _, _, _, err := loadSettingsConfig(ctx, base, repo); err == nil {
				t.Fatal("startup accepted invalid persisted retention")
			}

		})
	}
}

type retentionBatchObserver struct {
	repository.AuditRepository
	after func(int)
	calls int
}

func (r *retentionBatchObserver) DeleteOlderThan(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n, err := r.AuditRepository.DeleteOlderThan(ctx, cutoff, limit)
	r.calls++
	if err == nil && r.after != nil {
		r.after(r.calls)
	}
	return n, err
}

func TestAuditRetentionDurablePolicyIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			dbA, dbB := settingsDatabasePair(t, dialect)
			base := settingsFileBaseline(t)
			base.Audit.RetentionPeriod = config.Duration(24 * time.Hour)
			a := settingsServiceOn(t, dbA, base, nil, nil)
			b := settingsServiceOn(t, dbB, base, nil, nil)
			key := clientKeyModel{Name: "retention", Prefix: "retention", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8}
			if err := dbA.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			repo := NewAuditRepository(dbA)
			now := time.Now().UTC().Truncate(time.Microsecond)
			records := make([]auditdomain.Record, 1003)
			for i := range records {
				created := now.Add(-48 * time.Hour)
				if i == 1002 {
					created = now
				}
				records[i] = auditdomain.Record{EventID: fmt.Sprintf("evt_retention_%06d", i), RequestID: fmt.Sprintf("retention-%d", i), ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 1, CreatedAt: created,
					Attempts: []auditdomain.Attempt{{Number: 1, Source: auditdomain.AttemptSourceCredential, Stage: "response", StartedAt: created}},
				}
			}
			if err := repo.CreateBatch(ctx, records); err != nil {
				t.Fatal(err)
			}
			observed := &retentionBatchObserver{AuditRepository: repo}
			observed.after = func(batch int) {
				if batch == 1 {
					saveAuditRetention(t, a, "72h")
				}
			}
			worker := auditapp.NewService(observed, newTestAuditJournal(t, 8), nil, 4, time.Millisecond)
			deleted, err := worker.SweepRetention(ctx, b)
			if err != nil || deleted != 500 || observed.calls != 2 {
				t.Fatalf("policy expansion deleted=%d calls=%d err=%v", deleted, observed.calls, err)
			}
			if b.Get().Revision != 0 {
				t.Fatal("test must retain an unnotified stale process snapshot")
			}
			assertRows := func(want int) {
				t.Helper()
				if n := countRows(t, dbA, "request_audits"); n != want {
					t.Fatalf("audits=%d want=%d", n, want)
				}
				if n := countRows(t, dbA, "request_audit_attempts"); n != want {
					t.Fatalf("attempts=%d want=%d", n, want)
				}
				var stored clientKeyModel
				if err := dbA.db.First(&stored, key.ID).Error; err != nil {
					t.Fatal(err)
				}
				if stored.BilledUsageUSDTicks != 1003 {
					t.Fatalf("retention changed cumulative billing: %d", stored.BilledUsageUSDTicks)
				}
			}
			assertRows(503)
			observed.after = nil
			saveAuditRetention(t, a, "0")
			if n, err := worker.SweepRetention(ctx, b); err != nil || n != 0 {
				t.Fatalf("zero deleted=%d err=%v", n, err)
			}
			saveAuditRetention(t, a, "24h")
			// A real policy SQL read failure must prevent deleting with cached 24h.
			if err := dbA.db.Exec("ALTER TABLE runtime_settings RENAME TO retention_settings_unavailable").Error; err != nil {
				t.Fatal(err)
			}
			if n, err := worker.SweepRetention(ctx, b); err == nil || n != 0 {
				t.Fatalf("policy failure deleted=%d err=%v", n, err)
			}
			if err := dbA.db.Exec("ALTER TABLE retention_settings_unavailable RENAME TO runtime_settings").Error; err != nil {
				t.Fatal(err)
			}
			assertRows(503)
			// Inject failure after deleting attempts inside the actual SQL tx.
			const fault = "b03_audit_delete_failure"
			if err := dbA.db.Callback().Delete().Before("gorm:delete").Register(fault, func(tx *gorm.DB) {
				if tx.Statement.Table == "request_audits" {
					tx.AddError(errors.New("audit delete unavailable"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := worker.SweepRetention(ctx, b); err == nil || n != 0 {
				t.Fatalf("delete failure=%d err=%v", n, err)
			}
			if err := dbA.db.Callback().Delete().Remove(fault); err != nil {
				t.Fatal(err)
			}
			assertRows(503)
			// Both real connections can clean concurrently; lock contention may
			// end a sweep, and a subsequent bounded sweep must safely converge.
			workerB := auditapp.NewService(NewAuditRepository(dbB), newTestAuditJournal(t, 8), nil, 4, time.Millisecond)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = worker.SweepRetention(ctx, a) }()
			go func() { defer wg.Done(); _, _ = workerB.SweepRetention(ctx, b) }()
			wg.Wait()
			if _, err := worker.SweepRetention(ctx, b); err != nil {
				t.Fatal(err)
			}
			assertRows(1)
			// Fixed SQL cutoff protects equality as well as newer records.
			if n, err := repo.DeleteOlderThan(ctx, now, 500); err != nil || n != 0 {
				t.Fatalf("boundary deleted=%d err=%v", n, err)
			}
			if n, err := repo.DeleteOlderThan(ctx, now.Add(time.Microsecond), 500); err != nil || n != 1 {
				t.Fatalf("strictly older deleted=%d err=%v", n, err)
			}
			assertRows(0)
		})
	}
}
