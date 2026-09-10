package relational

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"gorm.io/gorm"
)

func capabilitySyncAccount(t *testing.T, db *Database) account.Credential {
	t.Helper()
	v, _, err := NewAccountRepository(db).UpsertByIdentity(context.Background(), account.Credential{Provider: account.ProviderBuild, Name: "capability-sync", SourceKey: "capability-sync", EncryptedAccessToken: "original", EncryptedRefreshToken: "refresh", AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func assertCapabilitySnapshot(t *testing.T, db *Database, id uint64, models []string) accountModelSyncStateModel {
	t.Helper()
	var names []string
	if err := db.db.Model(&accountModelCapabilityModel{}).Where("account_id = ?", id).Order("upstream_model").Pluck("upstream_model", &names).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, models) {
		t.Fatalf("capability snapshot = %v, want %v", names, models)
	}
	var state accountModelSyncStateModel
	if err := db.db.First(&state, "account_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return state
}

func TestCapabilitySyncOrderingAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			accounts := NewAccountRepository(a)
			v := capabilitySyncAccount(t, a)
			ra, rb := NewModelRepository(a), NewModelRepository(b)
			now := time.Now().UTC().Truncate(time.Microsecond)
			if err := testsupport.Capabilities(ctx, ra, accounts, v.ID, []string{"original"}, now); err != nil {
				t.Fatal(err)
			}
			var notifications atomic.Int32
			observe := func(ctx context.Context, event repository.InvalidationEvent) {
				if event.Kind != repository.InvalidationAccountCapabilityChanged {
					t.Errorf("unexpected notification %s", event.Kind)
				}
				var state accountModelSyncStateModel
				if err := b.db.WithContext(ctx).First(&state, "account_id = ?", v.ID).Error; err != nil || state.SyncPending {
					t.Errorf("notification before committed completion: pending=%t err=%v", state.SyncPending, err)
				}
				notifications.Add(1)
			}
			ra.SetInvalidationObserver(observe)
			rb.SetInvalidationObserver(observe)
			// Wall clock moves backwards: the persistent claim order still wins.
			old, err := ra.BeginAccountCapabilitySync(ctx, v.ID, now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			newer, err := rb.BeginAccountCapabilitySync(ctx, v.ID, now.Add(-time.Hour))
			if err != nil || newer.Revision != old.Revision+1 {
				t.Fatalf("claim order: %v %v %v", old, newer, err)
			}
			if err := rb.CompleteAccountCapabilitySync(ctx, newer, model.CapabilitySyncResult{Credential: v.CredentialRef(), Err: errors.New("")}); err != nil {
				t.Fatal(err)
			}
			for _, result := range []model.CapabilitySyncResult{{Credential: v.CredentialRef(), Models: []string{"late"}}, {Credential: v.CredentialRef(), Err: errors.New("late failure")}} {
				if err := ra.CompleteAccountCapabilitySync(ctx, old, result); !errors.Is(err, model.ErrCapabilitySyncSuperseded) {
					t.Fatalf("older observation accepted: %v", err)
				}
				if err := ra.CompleteAccountCapabilitySync(ctx, newer, result); !errors.Is(err, model.ErrCapabilitySyncSuperseded) {
					t.Fatalf("completed observation replayed: %v", err)
				}
			}
			state := assertCapabilitySnapshot(t, b, v.ID, []string{"original"})
			if state.SyncPending || state.LastSuccessAt == nil || !state.LastSuccessAt.Equal(now) || notifications.Load() != 0 {
				t.Fatal("failed/stale observations changed successful snapshot")
			}
			// Concurrent starts across pools must each get a distinct revision.
			const count = 20
			refs, errs := make(chan model.CapabilitySyncRef, count), make(chan error, count)
			var wg sync.WaitGroup
			for i := range count {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					ref, err := repo.BeginAccountCapabilitySync(ctx, v.ID, now)
					refs <- ref
					errs <- err
				}()
			}
			wg.Wait()
			close(refs)
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			seen := make(map[uint64]bool)
			var latest model.CapabilitySyncRef
			for ref := range refs {
				if seen[ref.Revision] {
					t.Fatal("claim revision reused")
				}
				seen[ref.Revision] = true
				if ref.Revision > latest.Revision {
					latest = ref
				}
			}
			if latest.Revision != newer.Revision+count {
				t.Fatalf("claim clock = %d", latest.Revision)
			}
			var accepted atomic.Int32
			errs = make(chan error, count)
			for i := range count {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					err := repo.CompleteAccountCapabilitySync(ctx, latest, model.CapabilitySyncResult{Credential: v.CredentialRef(), Models: []string{" newest ", "newest", ""}})
					if err == nil {
						accepted.Add(1)
					} else if !errors.Is(err, model.ErrCapabilitySyncSuperseded) {
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			if accepted.Load() != 1 || notifications.Load() != 1 {
				t.Fatalf("accepted=%d notifications=%d", accepted.Load(), notifications.Load())
			}
			state = assertCapabilitySnapshot(t, b, v.ID, []string{"newest"})
			if state.SyncPending || state.SyncRevision != latest.Revision || state.LastError != "" || !state.LastSuccessAt.Equal(now) {
				t.Fatal("completion state is inconsistent")
			}
			current, err := accounts.Get(ctx, v.ID)
			if err != nil || current.CredentialRef() != v.CredentialRef() || current.HealthRevision != v.HealthRevision || !current.UpdatedAt.Equal(v.UpdatedAt) {
				t.Fatal("model sync changed account-owned state")
			}
		})
	}
}

func TestCapabilitySyncMaterialAndTransactionalCompletion(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			v := capabilitySyncAccount(t, a)
			accounts := NewAccountRepository(b)
			ra, rb := NewModelRepository(a), NewModelRepository(b)
			now := time.Now().UTC().Truncate(time.Microsecond)
			if err := testsupport.Capabilities(ctx, ra, accounts, v.ID, []string{"original"}, now); err != nil {
				t.Fatal(err)
			}
			ref, err := ra.BeginAccountCapabilitySync(ctx, v.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			current, _, err := accounts.UpsertByIdentity(ctx, v)
			if err != nil {
				t.Fatal(err)
			}
			// No newer sync is required for material replacement to fence results.
			for _, result := range []model.CapabilitySyncResult{{Credential: v.CredentialRef(), Models: []string{"stale"}}, {Credential: v.CredentialRef(), Err: errors.New("stale")}} {
				if err := ra.CompleteAccountCapabilitySync(ctx, ref, result); !errors.Is(err, model.ErrCapabilitySyncSuperseded) {
					t.Fatalf("old material completed: %v", err)
				}
			}
			assertCapabilitySnapshot(t, b, v.ID, []string{"original"})
			ref, err = rb.BeginAccountCapabilitySync(ctx, v.ID, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			// The same observation can legitimately refresh before ListModels.
			refreshed, err := accounts.ApplyCredential(ctx, current.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRefreshed, AccessToken: "rotated", ExpiresAt: now.Add(time.Hour)})
			if err != nil || !refreshed.Applied {
				t.Fatalf("refresh = %t, %v", refreshed.Applied, err)
			}
			result := model.CapabilitySyncResult{Credential: refreshed.Credential.CredentialRef(), Models: []string{"newest"}}
			var notifications atomic.Int32
			ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { notifications.Add(1) })
			// Invalid rows must roll back the deletion and preserve a retryable claim.
			invalid := result
			invalid.Models = []string{strings.Repeat("x", 256)}
			if err := ra.CompleteAccountCapabilitySync(ctx, ref, invalid); err == nil {
				t.Fatal("invalid capability accepted")
			}
			state := assertCapabilitySnapshot(t, b, v.ID, []string{"original"})
			if !state.SyncPending || state.LastSuccessAt == nil || !state.LastSuccessAt.Equal(now) || notifications.Load() != 0 {
				t.Fatal("failed replacement was partially committed")
			}
			failure := errors.New("completion-state SQL unavailable")
			if err := a.db.Callback().Update().Before("gorm:update").Register("capability-final-write-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "account_model_sync_states" {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			err = ra.CompleteAccountCapabilitySync(ctx, ref, result)
			if removeErr := a.db.Callback().Update().Remove("capability-final-write-failure"); removeErr != nil {
				t.Fatal(removeErr)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("completion failure = %v", err)
			}
			state = assertCapabilitySnapshot(t, b, v.ID, []string{"original"})
			if !state.SyncPending || notifications.Load() != 0 {
				t.Fatal("final-state failure lost claim or notified")
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := ra.CompleteAccountCapabilitySync(cancelled, ref, result); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled completion = %v", err)
			}
			if err := ra.CompleteAccountCapabilitySync(ctx, ref, result); err != nil {
				t.Fatal(err)
			}
			assertCapabilitySnapshot(t, b, v.ID, []string{"newest"})
			if notifications.Load() != 1 {
				t.Fatal("successful retry did not notify once")
			}
			// Starting a later observation replaces an abandoned pending claim;
			// reconstructing the repository needs no process-local recovery state.
			abandoned, err := ra.BeginAccountCapabilitySync(ctx, v.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			restarted := NewModelRepository(b)
			ref, err = restarted.BeginAccountCapabilitySync(ctx, v.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := ra.CompleteAccountCapabilitySync(ctx, abandoned, result); !errors.Is(err, model.ErrCapabilitySyncSuperseded) {
				t.Fatal("abandoned claim regained ownership")
			}
			result.Models = []string{}
			if err := restarted.CompleteAccountCapabilitySync(ctx, ref, result); err != nil {
				t.Fatal(err)
			}
			state = assertCapabilitySnapshot(t, a, v.ID, []string{})
			if state.SyncPending || state.LastSuccessAt == nil {
				t.Fatal("successful empty snapshot was not committed")
			}
		})
	}
}

func TestCapabilitySyncLegacyMigrationAndRevisionExhaustion(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			v := capabilitySyncAccount(t, a)
			now := time.Now().UTC().Truncate(time.Microsecond)
			if err := a.db.Create(&accountModelCapabilityModel{AccountID: v.ID, UpstreamModel: "legacy"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Create(&accountModelSyncStateModel{AccountID: v.ID, LastAttemptAt: now, LastSuccessAt: &now, LastError: "legacy failure"}).Error; err != nil {
				t.Fatal(err)
			}
			drop := func() error {
				if err := a.db.Migrator().DropConstraint(&accountModelSyncStateModel{}, "chk_account_model_sync_revision"); err != nil {
					return err
				}
				for _, name := range []string{"sync_revision", "sync_pending"} {
					if err := a.db.Migrator().DropColumn(&accountModelSyncStateModel{}, name); err != nil {
						return err
					}
				}
				return nil
			}
			var err error
			if dialect == "sqlite" {
				err = a.withSQLiteForeignKeysDisabled(ctx, drop)
			} else {
				err = drop()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			state := assertCapabilitySnapshot(t, b, v.ID, []string{"legacy"})
			if state.SyncRevision != 0 || state.SyncPending || state.LastError != "legacy failure" || state.LastSuccessAt == nil || !state.LastSuccessAt.Equal(now) || !state.LastAttemptAt.Equal(now) {
				t.Fatal("migration altered legacy snapshot")
			}
			repo := NewModelRepository(b)
			ref, err := repo.BeginAccountCapabilitySync(ctx, v.ID, now.Add(time.Minute))
			if err != nil || ref.Revision != 1 {
				t.Fatalf("first upgraded claim = %v %v", ref, err)
			}
			if err := repo.CompleteAccountCapabilitySync(ctx, ref, model.CapabilitySyncResult{Credential: v.CredentialRef(), Models: []string{"upgraded"}}); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Model(&accountModelSyncStateModel{}).Where("account_id = ?", v.ID).Update("sync_revision", int64(-1)).Error; err == nil {
				t.Fatal("negative revision accepted")
			}
			if err := a.db.Model(&accountModelSyncStateModel{}).Where("account_id = ?", v.ID).Update("sync_revision", uint64(math.MaxInt64)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := repo.BeginAccountCapabilitySync(ctx, v.ID, now.Add(time.Hour)); !errors.Is(err, model.ErrCapabilitySyncExhausted) {
				t.Fatalf("revision overflow = %v", err)
			}
			state = assertCapabilitySnapshot(t, a, v.ID, []string{"upgraded"})
			if state.SyncRevision != math.MaxInt64 || state.SyncPending || !state.LastAttemptAt.Equal(now.Add(time.Minute)) {
				t.Fatal("overflow changed successful state")
			}
		})
	}
}
