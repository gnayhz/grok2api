package relational

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/guard"
)

func TestGuardSettingsAuthorityIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			aDB, bDB := settingsDatabasePair(t, dialect)
			aDocs, bDocs := NewSettingsDocumentRepository(aDB, guard.SettingsKey), NewSettingsDocumentRepository(bDB, guard.SettingsKey)
			file := guard.DefaultConfig()
			file.AccountCooldown = 168 * time.Hour
			a, b := guard.New(file, guard.NewDocumentStore(aDocs)), guard.New(file, guard.NewDocumentStore(bDocs))
			// Current legacy payload schema (numeric duration nanoseconds) is retained.
			legacy := []byte(`{"enabled":true,"guarded_models":["grok_build:grok-4.5","custom"],"max_attempts":7,"reasoning_expected":true,"evidence_timeout":0,"account_cooldown":60000000000}`)
			if _, err := aDocs.Save(ctx, legacy, 0); err != nil {
				t.Fatal(err)
			}
			observed, err := a.Read(ctx)
			if err != nil || observed.Revision != 1 || observed.MaxAttempts != 7 || observed.EvidenceTimeout != 3500*time.Millisecond {
				t.Fatalf("legacy read=%+v %v", observed, err)
			}
			if _, err := b.Read(ctx); err != nil {
				t.Fatal(err)
			}
			stale := b.Config()
			next := a.Config()
			next.MaxAttempts, next.CreatedTimeout = 100, 24*time.Hour
			saved, err := a.Update(ctx, next)
			if err != nil || saved.Revision != 2 || saved.MaxAttempts != 100 {
				t.Fatalf("save=%+v %v", saved, err)
			}
			if _, err := b.Update(ctx, stale); !errors.Is(err, guard.ErrConflict) {
				t.Fatalf("stale replica=%v", err)
			}
			if _, err := b.Read(ctx); err != nil {
				t.Fatal(err)
			}
			if b.Config().MaxAttempts != 100 {
				t.Fatal("peer failed to reconcile without notification")
			}
			if _, err := b.ResetToDefaults(ctx, 1); !errors.Is(err, guard.ErrConflict) {
				t.Fatalf("stale reset=%v", err)
			}
			reset, err := b.ResetToDefaults(ctx, 2)
			if err != nil || reset.Revision != 3 || reset.AccountCooldown != file.AccountCooldown {
				t.Fatalf("scoped reset=%+v %v", reset, err)
			}
			restarted := guard.New(file, guard.NewDocumentStore(aDocs))
			if _, err := restarted.Read(ctx); err != nil || restarted.Config().Revision != 3 || restarted.Config().MaxAttempts != file.MaxAttempts {
				t.Fatalf("restart=%v %+v", err, restarted.Config())
			}
			gateway, err := NewSettingsDocumentRepository(aDB, "gateway").Load(ctx)
			if err != nil || gateway.Revision != 0 {
				t.Fatal("guard touched gateway settings")
			}
			// Two real connections racing the same observed CAS produce one winner.
			if _, err := a.Read(ctx); err != nil {
				t.Fatal(err)
			}
			ca, cb := a.Config(), b.Config()
			ca.MaxAttempts, cb.MaxAttempts = 4, 5
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for _, run := range []func() error{
				func() error { _, e := a.Update(ctx, ca); return e },
				func() error { _, e := b.Update(ctx, cb); return e },
			} {
				wg.Add(1)
				go func(f func() error) { defer wg.Done(); results <- f() }(run)
			}
			wg.Wait()
			close(results)
			successes, conflicts := 0, 0
			for e := range results {
				if e == nil {
					successes++
				} else if errors.Is(e, guard.ErrConflict) {
					conflicts++
				} else {
					t.Fatal(e)
				}
			}
			if successes != 1 || conflicts != 1 {
				t.Fatalf("CAS wins=%d conflicts=%d", successes, conflicts)
			}
			if _, err := a.Read(ctx); err != nil {
				t.Fatal(err)
			}
			// Revision regression invalidates the request snapshot and cannot replace it.
			doc, err := aDocs.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := aDB.db.Model(&runtimeSettingsModel{}).Where("key = ?", guard.SettingsKey).Update("revision", 2).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := a.Read(ctx); err == nil {
				t.Fatal("accepted policy regression")
			}
			if cfg, err := a.Snapshot(); err == nil || cfg.Revision != 4 {
				t.Fatal("regression did not fail closed")
			}
			if err := aDB.db.Model(&runtimeSettingsModel{}).Where("key = ?", guard.SettingsKey).Update("revision", doc.Revision).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := a.Read(ctx); err != nil {
				t.Fatal(err)
			}
			// Check clock precision and overflow using the actual SQL int64 range.
			if err := aDB.db.Model(&runtimeSettingsModel{}).Where("key = ?", guard.SettingsKey).Update("revision", int64(9007199254740993)).Error; err != nil {
				t.Fatal(err)
			}
			large, err := a.Read(ctx)
			if err != nil || large.Revision != 9007199254740993 {
				t.Fatalf("large revision=%+v %v", large, err)
			}
			if large, err = a.Update(ctx, large); err != nil || large.Revision != 9007199254740994 {
				t.Fatalf("large CAS=%+v %v", large, err)
			}
			if err := aDB.db.Model(&runtimeSettingsModel{}).Where("key = ?", guard.SettingsKey).Update("revision", int64(math.MaxInt64)).Error; err != nil {
				t.Fatal(err)
			}
			max, err := a.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Update(ctx, max); err == nil {
				t.Fatal("overflowed durable clock")
			}
			before := a.Config()
			// An invalid stored policy also invalidates request snapshots; it never becomes
			// an advertised fresh management config or a silently disabled guard.
			var broken map[string]any
			if err := json.Unmarshal(doc.Payload, &broken); err != nil {
				t.Fatal(err)
			}
			broken["guarded_models"] = []string{"unknown:bad"}
			payload, _ := json.Marshal(broken)
			if err := aDB.db.Model(&runtimeSettingsModel{}).Where("key = ?", guard.SettingsKey).Update("value_json", string(payload)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := a.Read(ctx); err == nil || a.Config().Revision != before.Revision {
				t.Fatal("invalid persisted policy replaced authority")
			}
			if err := bDB.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Read(ctx); err == nil {
				t.Fatal("storage unavailable advertised as fresh")
			}
		})
	}
}

func TestGuardBootstrapReadinessIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			aDB, bDB := settingsDatabasePair(t, dialect)
			aStore := guard.NewDocumentStore(NewSettingsDocumentRepository(aDB, guard.SettingsKey))
			bStore := guard.NewDocumentStore(NewSettingsDocumentRepository(bDB, guard.SettingsKey))
			broken := guard.DefaultConfig()
			broken.Enabled, broken.EvidenceTimeout = false, -time.Second
			first := guard.New(broken, aStore)
			for i := 0; i < 2; i++ {
				if _, err := first.Read(ctx); err == nil {
					t.Fatal("invalid bootstrap became ready without row")
				}
				if _, err := first.Snapshot(); err == nil {
					t.Fatal("read cleared bootstrap error")
				}
			}
			// An explicit durable policy supersedes the invalid legacy fallback.
			writer := guard.New(guard.DefaultConfig(), bStore)
			valid := writer.Config()
			valid.CreatedTimeout, valid.AdmissionTimeout = 700*time.Millisecond, 12*time.Minute
			saved, err := writer.Update(ctx, valid)
			if err != nil {
				t.Fatal(err)
			}
			active, err := first.Read(ctx)
			if err != nil || active.Revision != saved.Revision || active.CreatedTimeout != saved.CreatedTimeout {
				t.Fatalf("durable repair=%+v %v", active, err)
			}
			if _, err := first.ResetToDefaults(ctx, active.Revision); !errors.Is(err, guard.ErrInvalidInput) {
				t.Fatalf("invalid reset=%v", err)
			}
			restarted := guard.New(broken, aStore)
			if active, err = restarted.Read(ctx); err != nil || active.Revision != saved.Revision || active.AdmissionTimeout != 12*time.Minute {
				t.Fatalf("restart=%+v %v", active, err)
			}
		})
	}
}
