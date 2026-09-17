package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/investigator"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type qualityRuntimeFixture struct {
	Court        *court.Service
	Investigator *investigator.Service
	Evidence     *evidence.Store
}

func qualityServiceOn(t *testing.T, db *Database) (*management.Service, qualityRuntimeFixture) {
	t.Helper()
	// 建表走 registry 的统一迁移语义(生产由 registry.New 完成;evidence.New
	// 不再自跑 AutoMigrate)。
	if err := db.db.AutoMigrate(evidence.Models()...); err != nil {
		t.Fatal(err)
	}
	ev, err := evidence.New(context.Background(), db.db, qualitymodel.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	runtime := qualityRuntimeFixture{
		Court:        court.New(court.DefaultConfig(), nil, ev, nil, nil),
		Investigator: investigator.New(investigator.DefaultConfig(), nil, nil), Evidence: ev,
	}
	t.Cleanup(func() { _ = runtime.Court.Close(context.Background()) })
	service := management.New(NewSettingsDocumentRepository(db, management.SettingsKey), (management.Runtime{Court: runtime.Court, Investigator: runtime.Investigator, Evidence: runtime.Evidence}).Apply, nil)
	if err := service.ReloadPersisted(context.Background()); err != nil {
		t.Fatal(err)
	}
	return service, runtime
}

func TestQualitySettingsDurableApplyIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			dbA, dbB := settingsDatabasePair(t, dialect)
			a, runtimeA := qualityServiceOn(t, dbA)
			b, runtimeB := qualityServiceOn(t, dbB)
			before := a.Snapshot()
			input := before.Config
			input.AccountNeedExits, input.AccountSpanNodes = 5, 3
			input.ExitNeedN, input.ExitNeedK, input.JurorsPerExit = 4, 3, 4
			input.DifferentialExits, input.ProbeBudget = 3, 9
			input.Retention, input.EvidenceWindow, input.InvestigationTimeout = "48h", "1h", "2m"
			saved, err := a.Update(ctx, before.Revision, input)
			if err != nil || saved.ApplyPending || saved.Revision != 1 || saved.AppliedRevision != 1 {
				t.Fatalf("save=%+v err=%v", saved, err)
			}
			if runtimeA.Court.Config().AccountNeedExits != 5 || runtimeA.Investigator.Config().ProbeBudget != 9 || runtimeA.Evidence.Config().Retention != 48*time.Hour {
				t.Fatal("runtime consumers did not apply")
			}
			if err := b.ReloadPersisted(ctx); err != nil {
				t.Fatal(err)
			}
			if b.Snapshot().Config != saved.Config || runtimeB.Evidence.Config().Window != time.Hour {
				t.Fatal("peer did not converge without a notification")
			}
			restarted, _ := qualityServiceOn(t, dbB)
			if restarted.Snapshot().Revision != 1 || restarted.Snapshot().Config != saved.Config {
				t.Fatal("restart lost durable intent")
			}

			// Actual evidence storage failure after the settings write must leave a
			// pending revision and must not blindly persist an older configuration.
			if err := dbA.db.Exec("DROP TABLE q_observation").Error; err != nil {
				t.Fatal(err)
			}
			input = saved.Config
			input.EvidenceWindow, input.ProbeBudget = "2h", 10
			pending, err := a.Update(ctx, saved.Revision, input)
			if err != nil || !pending.ApplyPending || pending.ApplyError == "" || pending.Revision != 2 || pending.AppliedRevision != 1 {
				t.Fatalf("pending=%+v err=%v", pending, err)
			}
			if runtimeA.Evidence.Config().Window != time.Hour || runtimeA.Investigator.Config().ProbeBudget != 9 {
				t.Fatal("failed evidence apply leaked later runtime changes")
			}
			// B can save and apply a newer revision while A is pending. A's recovery
			// must read that newer policy, not roll back B or replay stale policy.
			input = saved.Config
			input.ProbeBudget = 11
			peer, err := b.Update(ctx, pending.Revision, input)
			if err != nil || peer.ApplyPending || peer.Revision != 3 {
				t.Fatalf("peer=%+v err=%v", peer, err)
			}
			if err := dbA.db.AutoMigrate(evidence.Models()...); err != nil {
				t.Fatal(err)
			}
			if err := a.ReloadPersisted(ctx); err != nil {
				t.Fatal(err)
			}
			got := a.Snapshot()
			if got.ApplyPending || got.Revision != 3 || got.AppliedRevision != 3 || runtimeA.Investigator.Config().ProbeBudget != 11 {
				t.Fatalf("recovery=%+v", got)
			}
			// Retry an unchanged saved revision after the failure is removed.
			if err := dbA.db.Exec("DROP TABLE q_observation").Error; err != nil {
				t.Fatal(err)
			}
			input = got.Config
			input.EvidenceWindow = "3h"
			pending, err = a.Update(ctx, got.Revision, input)
			if err != nil || !pending.ApplyPending {
				t.Fatalf("pending=%+v err=%v", pending, err)
			}
			if err := dbA.db.AutoMigrate(evidence.Models()...); err != nil {
				t.Fatal(err)
			}
			if err := a.ReloadPersisted(ctx); err != nil {
				t.Fatal(err)
			}
			if a.Snapshot().AppliedRevision != pending.Revision || runtimeA.Evidence.Config().Window != 3*time.Hour {
				t.Fatal("same-revision retry failed")
			}

			for round := 0; round < 8; round++ {
				if err := a.ReloadPersisted(ctx); err != nil {
					t.Fatal(err)
				}
				if err := b.ReloadPersisted(ctx); err != nil {
					t.Fatal(err)
				}
				before := a.Snapshot()
				one, two := before.Config, before.Config
				one.ProbeBudget, two.ProbeBudget = 20+round, 40+round
				start, results := make(chan struct{}), make(chan error, 2)
				go func() { <-start; _, err := a.Update(ctx, before.Revision, one); results <- err }()
				go func() { <-start; _, err := b.Update(ctx, before.Revision, two); results <- err }()
				close(start)
				wins, conflicts := 0, 0
				for range 2 {
					err := <-results
					if err == nil {
						wins++
					} else if errors.Is(err, repository.ErrConflict) {
						conflicts++
					} else {
						t.Fatal(err)
					}
				}
				if wins != 1 || conflicts != 1 {
					t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
				}
			}
			if err := a.ReloadPersisted(ctx); err != nil {
				t.Fatal(err)
			}
			before = a.Snapshot()
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := a.Update(canceled, before.Revision, before.Config); err == nil {
				t.Fatal("canceled write succeeded")
			}
			bad := before.Config
			bad.Retention = "invalid"
			if _, err := a.Update(ctx, before.Revision, bad); !errors.Is(err, management.ErrInvalidInput) {
				t.Fatalf("invalid write=%v", err)
			}
			if a.Snapshot() != before {
				t.Fatal("rejected write changed state")
			}
			t.Logf("two SQL connections, real evidence/court/investigator apply, failure recovery, restart, cancellation and eight CAS rounds passed; revision=%d", before.Revision)
		})
	}
}

func TestQualitySettingsConcurrentApplyFailureDoesNotRollback(t *testing.T) {
	dbA, dbB := settingsDatabasePair(t, "sqlite")
	entered, finish := make(chan struct{}), make(chan struct{})
	var fail atomic.Bool
	a := management.New(NewSettingsDocumentRepository(dbA, management.SettingsKey), func(context.Context, management.Config) error {
		if fail.Load() {
			close(entered)
			<-finish
			return errors.New("interrupted apply")
		}
		return nil
	}, nil)
	b, _ := qualityServiceOn(t, dbB)
	ctx := context.Background()
	if err := a.ReloadPersisted(ctx); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	done := make(chan error, 1)
	go func() { _, err := a.Update(ctx, 0, management.DefaultConfig()); done <- err }()
	<-entered
	input := management.DefaultConfig()
	input.ProbeBudget++
	saved, err := b.Update(ctx, 1, input)
	close(finish)
	if err != nil || saved.Revision != 2 {
		t.Fatalf("peer save=%+v %v", saved, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	fail.Store(false)
	if err := a.ReloadPersisted(ctx); err != nil {
		t.Fatal(err)
	}
	if got := a.Snapshot(); got.Revision != 2 || got.Config.ProbeBudget != input.ProbeBudget {
		t.Fatalf("peer overwritten: %+v", got)
	}
}

func TestQualityRotationCapacityMigrationIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, baselineKind := range []string{"absent", "override", "reset"} {
			t.Run(dialect+"/"+baselineKind, func(t *testing.T) {
				ctx := context.Background()
				dbA, dbB := settingsDatabasePair(t, dialect)
				base := settingsFileBaseline(t)
				general := settingsServiceOn(t, dbA, base, nil, nil)
				expectedRev := uint64(1)
				if baselineKind != "absent" {
					input := general.Get()
					input.Config.Server.MaxConcurrentRequests = 2345
					input.Config.EgressRotation.MaxGlobalPerHour = 8
					if _, err := general.Update(ctx, input.Revision, input.Config); err != nil {
						t.Fatal(err)
					}
					expectedRev++
				}
				if baselineKind == "reset" {
					if _, err := general.ResetToDefaults(ctx, general.Get().Revision); err != nil {
						t.Fatal(err)
					}
					expectedRev++
				}
				legacy := map[string]any{"max_rotations_per_hour": 12, "probe_budget": 14}
				payload, _ := json.Marshal(legacy)
				if _, err := NewSettingsDocumentRepository(dbA, management.SettingsKey).Save(ctx, payload, 0); err != nil {
					t.Fatal(err)
				}
				cipher, _ := security.NewCipher(base.Secrets.CredentialEncryptionKey)
				repoA, repoB := NewRuntimeSettingsRepository(dbA, cipher), NewRuntimeSettingsRepository(dbB, cipher)
				start, results := make(chan struct{}), make(chan error, 2)
				for _, repo := range []*RuntimeSettingsRepository{repoA, repoB} {
					go func() {
						<-start
						results <- settingsapp.MigrateLegacyQualityRotation(ctx, config.ToRuntimeSettings(base), repo)
					}()
				}
				close(start)
				for range 2 {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
				loaded := settingsServiceOn(t, dbB, base, nil, nil)
				if got := loaded.Get(); got.Revision != expectedRev || got.Config.EgressRotation.MaxGlobalPerHour != 12 {
					t.Fatalf("migration=%+v", got)
				}
				wantRequests := base.Server.MaxConcurrentRequests
				if baselineKind == "override" {
					wantRequests = 2345
				}
				if loaded.Get().Config.Server.MaxConcurrentRequests != wantRequests {
					t.Fatal("unrelated setting lost")
				}
				doc, err := NewSettingsDocumentRepository(dbA, management.SettingsKey).Load(ctx)
				if err != nil || doc.Revision != 2 {
					t.Fatalf("quality clock=%+v %v", doc, err)
				}
				var fields map[string]any
				_ = json.Unmarshal(doc.Payload, &fields)
				if _, exists := fields["max_rotations_per_hour"]; exists || fields["probe_budget"] != float64(14) {
					t.Fatal("migration marker or quality policy lost")
				}
				// A new gateway save and a subsequent reset cannot be undone on startup.
				input := loaded.Get()
				input.Config.EgressRotation.MaxGlobalPerHour = 9
				if _, err := loaded.Update(ctx, input.Revision, input.Config); err != nil {
					t.Fatal(err)
				}
				if err := settingsapp.MigrateLegacyQualityRotation(ctx, config.ToRuntimeSettings(base), repoA); err != nil {
					t.Fatal(err)
				}
				if settingsServiceOn(t, dbA, base, nil, nil).Get().Config.EgressRotation.MaxGlobalPerHour != 9 {
					t.Fatal("legacy capacity re-applied")
				}
				if _, err := loaded.ResetToDefaults(ctx, loaded.Get().Revision); err != nil {
					t.Fatal(err)
				}
				if err := settingsapp.MigrateLegacyQualityRotation(ctx, config.ToRuntimeSettings(base), repoB); err != nil {
					t.Fatal(err)
				}
				if settingsServiceOn(t, dbB, base, nil, nil).Get().Config.EgressRotation.MaxGlobalPerHour != base.Egress.Rotation.MaxGlobalPerHour {
					t.Fatal("reset did not restore sole owner baseline")
				}
			})
		}
	}
}

func TestQualitySettingsRedisReconcileIntegration(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	dialect := "sqlite"
	if os.Getenv("TEST_POSTGRES_DSN") != "" {
		dialect = "postgres"
	}
	dbA, dbB := settingsDatabasePair(t, dialect)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := redisruntime.Config{Address: address, KeyPrefix: fmt.Sprintf("quality-settings-%d", time.Now().UnixNano())}
	busA, err := redisruntime.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer busA.Close()
	busB, err := redisruntime.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer busB.Close()
	a, _ := qualityServiceOn(t, dbA)
	b, runtimeB := qualityServiceOn(t, dbB)
	heard := make(chan struct{}, 8)
	listenCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- busB.ListenSettingsChanges(listenCtx, func(ctx context.Context) error {
			if err := b.ReloadPersisted(ctx); err != nil {
				return err
			}
			select {
			case heard <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	stopListening := sync.OnceFunc(func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	defer stopListening()
	ready := false
	for !ready {
		if err := busA.PublishSettingsChanged(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-heard:
			ready = true
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	input := a.Snapshot().Config
	input.ProbeBudget = 16
	if _, err := a.Update(ctx, 0, input); err != nil {
		t.Fatal(err)
	}
	if err := busA.PublishSettingsChanged(ctx); err != nil {
		t.Fatal(err)
	}
	for b.Snapshot().Revision != 1 {
		select {
		case <-heard:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if runtimeB.Investigator.Config().ProbeBudget != 16 {
		t.Fatal("notification failed to apply")
	}
	stopListening()
	input.ProbeBudget = 17
	if _, err := a.Update(ctx, 1, input); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadPersisted(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Snapshot().Revision != 2 || runtimeB.Investigator.Config().ProbeBudget != 17 {
		t.Fatal("lost notification prevented convergence")
	}
	t.Log("real Redis notification and notification-free SQL reconciliation passed")
}

func TestQualityRotationMigrationLegacyDefaultsAndNewDocuments(t *testing.T) {
	for _, payload := range []string{
		`{"account_need_exits":0,"retention":"","probe_budget":14}`,
		`{"max_rotations_per_hour":0,"probe_budget":14}`,
	} {
		t.Run(payload, func(t *testing.T) {
			db, _ := settingsDatabasePair(t, "sqlite")
			ctx := context.Background()
			base := settingsFileBaseline(t)
			base.Egress.Rotation.MaxGlobalPerHour = 20
			cipher, _ := security.NewCipher(base.Secrets.CredentialEncryptionKey)
			repo := NewRuntimeSettingsRepository(db, cipher)
			if _, err := NewSettingsDocumentRepository(db, management.SettingsKey).Save(ctx, []byte(payload), 0); err != nil {
				t.Fatal(err)
			}
			if err := settingsapp.MigrateLegacyQualityRotation(ctx, config.ToRuntimeSettings(base), repo); err != nil {
				t.Fatal(err)
			}
			if settingsServiceOn(t, db, base, nil, nil).Get().Config.EgressRotation.MaxGlobalPerHour != 6 {
				t.Fatal("legacy implicit capacity default lost")
			}
			quality, _ := qualityServiceOn(t, db)
			if quality.Snapshot().Config.ProbeBudget != 14 || quality.Snapshot().Config.AccountNeedExits != management.DefaultConfig().AccountNeedExits {
				t.Fatal("legacy parameter defaults changed")
			}
		})
	}
	db, _ := settingsDatabasePair(t, "sqlite")
	ctx := context.Background()
	base := settingsFileBaseline(t)
	base.Egress.Rotation.MaxGlobalPerHour = 20
	quality, _ := qualityServiceOn(t, db)
	if _, err := quality.Update(ctx, 0, management.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	cipher, _ := security.NewCipher(base.Secrets.CredentialEncryptionKey)
	if err := settingsapp.MigrateLegacyQualityRotation(ctx, config.ToRuntimeSettings(base), NewRuntimeSettingsRepository(db, cipher)); err != nil {
		t.Fatal(err)
	}
	if got := settingsServiceOn(t, db, base, nil, nil).Get(); got.Revision != 0 || got.Config.EgressRotation.MaxGlobalPerHour != 20 {
		t.Fatal("new quality document created a legacy capacity override")
	}
}

func TestQualityRotationMigrationRollsBackBothDocuments(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, _ := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			base := settingsFileBaseline(t)
			cipher, _ := security.NewCipher(base.Secrets.CredentialEncryptionKey)
			repo := NewRuntimeSettingsRepository(db, cipher)
			documents := NewSettingsDocumentRepository(db, management.SettingsKey)
			original, err := documents.Save(ctx, []byte(`{"max_rotations_per_hour":12,"probe_budget":14}`), 0)
			if err != nil {
				t.Fatal(err)
			}
			callback := "test:fail_quality_migration_marker"
			if err := db.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				values, ok := tx.Statement.Dest.(map[string]any)
				if ok && strings.Contains(fmt.Sprint(values["value_json"]), "_network_capacity_migrated") {
					tx.AddError(errors.New("injected migration marker write failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			err = settingsapp.MigrateLegacyQualityRotation(ctx, config.ToRuntimeSettings(base), repo)
			_ = db.db.Callback().Update().Remove(callback)
			if err == nil {
				t.Fatal("migration unexpectedly succeeded")
			}
			_, _, revision, found, err := repo.Get(ctx)
			if err != nil || found || revision != 0 {
				t.Fatalf("gateway half of migration committed: rev=%d found=%v err=%v", revision, found, err)
			}
			document, err := documents.Load(ctx)
			if err != nil || document.Revision != original.Revision || string(document.Payload) != string(original.Payload) {
				t.Fatal("quality half changed during failed migration")
			}
			if err := settingsapp.MigrateLegacyQualityRotation(ctx, config.ToRuntimeSettings(base), repo); err != nil {
				t.Fatal(err)
			}
		})
	}
}
