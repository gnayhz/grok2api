package relational

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAccountWebProfileObservationUsesLockedMaterial(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v := importFixture(t, ra, account.ProviderWeb, "profile", "profile@example.test")
			if err := a.db.Model(&accountCredentialModel{}).Where("account_id = ?", v.ID).Update("generation", 0).Error; err != nil {
				t.Fatal(err)
			}
			v.CredentialGeneration = 0
			var notifications atomic.Int32
			ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { notifications.Add(1) })
			now := time.Now().UTC().Truncate(time.Microsecond)
			for _, kind := range []account.WebProfileEventKind{account.WebProfileNSFWEnabled, account.WebProfileTermsAccepted, account.WebProfileBirthDateSet} {
				event := account.WebProfileObservation{Kind: kind, OccurredAt: now, TermsVersion: account.CurrentWebTermsVersion}
				result, err := ra.ApplyWebProfile(ctx, v.CredentialRef(), event)
				if err != nil || !result.Applied || !result.Changed || result.State.Material != v.CredentialRef() {
					t.Fatalf("current legacy event %s not committed: %+v %v", kind, result, err)
				}
				before := notifications.Load()
				event.OccurredAt = now.Add(time.Hour)
				result, err = ra.ApplyWebProfile(ctx, v.CredentialRef(), event)
				if err != nil || !result.Applied || result.Changed || notifications.Load() != before {
					t.Fatalf("repeated %s was not idempotent: %v", kind, err)
				}
			}
			current, err := rb.Get(ctx, v.ID)
			if err != nil || current.WebNSFWEnabledAt == nil || !current.WebNSFWEnabledAt.Equal(now) || current.WebTermsAcceptedAt == nil || !current.WebTermsAcceptedAt.Equal(now) || current.WebBirthDateSetAt == nil || !current.WebBirthDateSetAt.Equal(now) {
				t.Fatalf("first timestamps lost: %v", err)
			}
			upgraded := account.WebProfileObservation{Kind: account.WebProfileTermsAccepted, OccurredAt: now.Add(time.Minute), TermsVersion: account.CurrentWebTermsVersion + 1}
			if result, err := ra.ApplyWebProfile(ctx, v.CredentialRef(), upgraded); err != nil || !result.Changed {
				t.Fatalf("terms upgrade=%+v err=%v", result, err)
			}
			upgraded.TermsVersion--
			if result, err := ra.ApplyWebProfile(ctx, v.CredentialRef(), upgraded); err != nil || result.Changed || result.State.TermsAcceptedVersion != account.CurrentWebTermsVersion+1 {
				t.Fatalf("terms downgrade=%+v err=%v", result, err)
			}
			for _, event := range []account.WebProfileObservation{{Kind: account.WebProfileNSFWEnabled}, {Kind: "unexpected", OccurredAt: now}, {Kind: account.WebProfileTermsAccepted, OccurredAt: now}} {
				before := notifications.Load()
				if _, err := ra.ApplyWebProfile(ctx, v.CredentialRef(), event); err == nil {
					t.Fatal("invalid observation accepted")
				}
				if notifications.Load() != before {
					t.Fatal("invalid observation notified")
				}
			}
			// Profile fields are independently owned from administrator intent,
			// health, quota tier and credential material.
			before, err := rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			disabled := false
			if _, err := rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &disabled}}); err != nil {
				t.Fatal(err)
			}
			if _, err := ra.ApplyWebProfile(ctx, v.CredentialRef(), account.WebProfileObservation{Kind: account.WebProfileNSFWEnabled, OccurredAt: now}); err != nil {
				t.Fatal(err)
			}
			after, err := rb.Get(ctx, v.ID)
			if err != nil || after.Enabled || after.AuthStatus != before.AuthStatus || after.HealthRevision != before.HealthRevision || after.WebTier != before.WebTier || after.EncryptedAccessToken != before.EncryptedAccessToken {
				t.Fatalf("profile changed another state dimension: %v", err)
			}
			replacement := v
			replacement.UserID = "different-known-user"
			replacement.EncryptedAccessToken = "replacement"
			replaced, _, err := rb.UpsertByIdentity(ctx, replacement)
			if err != nil {
				t.Fatal(err)
			}
			count := notifications.Load()
			result, err := ra.ApplyWebProfile(ctx, v.CredentialRef(), account.WebProfileObservation{Kind: account.WebProfileNSFWEnabled, OccurredAt: now})
			if err != nil || result.Applied || result.Changed || result.State.Material != replaced.CredentialRef() || notifications.Load() != count {
				t.Fatalf("stale observation crossed replacement: %+v %v", result, err)
			}
			wrong := replaced.CredentialRef()
			wrong.Provider = account.ProviderConsole
			if _, err := ra.ApplyWebProfile(ctx, wrong, account.WebProfileObservation{}); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("wrong provider=%v", err)
			}
			if err := rb.Delete(ctx, replaced.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := ra.ApplyWebProfile(ctx, replaced.CredentialRef(), account.WebProfileObservation{Kind: account.WebProfileNSFWEnabled, OccurredAt: now}); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("deleted observation=%v", err)
			}
		})
	}
}

func TestAccountWebProfileRollbackAndConcurrentImport(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v := importFixture(t, ra, account.ProviderWeb, "rollback-profile", "rollback-profile@example.test")
			injected := errors.New("injected profile commit failure")
			callback := "g23_profile_rollback"
			if err := a.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "web_account_profiles" {
					_ = tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			result, err := ra.ApplyWebProfile(ctx, v.CredentialRef(), account.WebProfileObservation{Kind: account.WebProfileNSFWEnabled, OccurredAt: time.Now()})
			if err := a.db.Callback().Create().Remove(callback); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(err, injected) || result.Applied || result.Changed {
				t.Fatalf("failed commit reported success: %+v %v", result, err)
			}
			current, err := rb.Get(ctx, v.ID)
			if err != nil || current.WebNSFWEnabledAt != nil {
				t.Fatalf("failed profile write persisted: %v", err)
			}
			for iteration := range 8 {
				old := importFixture(t, ra, account.ProviderWeb, fmt.Sprintf("race-%d", iteration), fmt.Sprintf("race-%d@example.test", iteration))
				old.UserID = fmt.Sprintf("old-%d", iteration)
				old, _, err = ra.UpsertByIdentity(ctx, old)
				if err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				errs := make(chan error, 2)
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, err := ra.ApplyWebProfile(ctx, old.CredentialRef(), account.WebProfileObservation{Kind: account.WebProfileNSFWEnabled, OccurredAt: time.Now()})
					errs <- err
				}()
				go func() {
					defer wg.Done()
					<-start
					replacement := old
					replacement.UserID = fmt.Sprintf("new-%d", iteration)
					replacement.EncryptedAccessToken = "replacement"
					_, _, err := rb.UpsertByIdentity(ctx, replacement)
					errs <- err
				}()
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
				current, err := rb.Get(ctx, old.ID)
				if err != nil || current.WebNSFWEnabledAt != nil || current.CredentialGeneration != old.CredentialGeneration+1 || current.UserID != fmt.Sprintf("new-%d", iteration) {
					t.Fatalf("old marker crossed atomic identity replacement: %v", err)
				}
			}
		})
	}
}

func TestAccountWebProfileIdentityResetIsAtomic(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			for _, path := range []string{"import", "identity"} {
				t.Run(path, func(t *testing.T) {
					now := time.Now().UTC().Truncate(time.Microsecond)
					oldUID, newUID := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
					v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: path, SourceKey: path, UserID: oldUID, EncryptedAccessToken: "old", WebTier: account.WebTierHeavy, WebNSFWEnabledAt: &now, WebBirthDateSetAt: &now, WebTermsAcceptedAt: &now, WebTermsAcceptedVersion: account.CurrentWebTermsVersion})
					if err != nil {
						t.Fatal(err)
					}
					disabled := false
					if _, err := ra.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &disabled}}); err != nil {
						t.Fatal(err)
					}
					if err := a.db.Model(&webAccountProfileModel{}).Where("account_id = ?", v.ID).Update("egress_identity", "kept-egress").Error; err != nil {
						t.Fatal(err)
					}
					if path == "identity" {
						// The late link write fails after profile reset and identity
						// update have already run within this transaction.
						build := importFixture(t, ra, account.ProviderBuild, "identity-build", "")
						if err := a.db.Model(&accountModel{}).Where("id = ?", build.ID).Update("user_id", newUID).Error; err != nil {
							t.Fatal(err)
						}
					}
					var notifications atomic.Int32
					ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { notifications.Add(1) })
					defer ra.SetInvalidationObserver(nil)
					fault := errors.New("profile identity transaction failed")
					callback := "g23_profile_identity_rollback"
					failTable := "web_account_profiles"
					if path == "identity" {
						failTable = "account_provider_links"
					}
					if err := a.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Table == failTable {
							_ = tx.AddError(fault)
						}
					}); err != nil {
						t.Fatal(err)
					}
					apply := func(explicit bool) error {
						if path == "identity" {
							result, err := ra.ApplyIdentity(ctx, v.CredentialRef(), account.IdentityObservation{UserID: newUID})
							if err != nil && result.Applied {
								return fmt.Errorf("failed identity reported applied: %w", err)
							}
							return err
						}
						replacement := v
						replacement.UserID, replacement.EncryptedAccessToken = newUID, "new"
						replacement.WebNSFWEnabledAt, replacement.WebBirthDateSetAt, replacement.WebTermsAcceptedAt = nil, nil, nil
						replacement.WebTermsAcceptedVersion = 0
						if explicit {
							replacement.UserID = "33333333-3333-4333-8333-333333333333"
							at := now.Add(time.Hour)
							replacement.WebNSFWEnabledAt, replacement.WebBirthDateSetAt, replacement.WebTermsAcceptedAt = &at, &at, &at
							replacement.WebTermsAcceptedVersion = account.CurrentWebTermsVersion
						}
						_, _, err := ra.UpsertByIdentity(ctx, replacement)
						return err
					}
					err = apply(false)
					if err := a.db.Callback().Create().Remove(callback); err != nil {
						t.Fatal(err)
					}
					if !errors.Is(err, fault) || notifications.Load() != 0 {
						t.Fatalf("failed transaction=%v events=%d", err, notifications.Load())
					}
					stored, err := rb.Get(ctx, v.ID)
					if err != nil || stored.UserID != oldUID || stored.CredentialRef() != v.CredentialRef() || stored.WebNSFWEnabledAt == nil || !stored.WebNSFWEnabledAt.Equal(now) || stored.WebBirthDateSetAt == nil || stored.WebTermsAcceptedAt == nil || stored.WebTermsAcceptedVersion != account.CurrentWebTermsVersion {
						t.Fatalf("failed identity reset leaked: %v", err)
					}
					if err := apply(false); err != nil {
						t.Fatal(err)
					}
					stored, err = rb.Get(ctx, v.ID)
					if err != nil || stored.UserID != newUID || stored.WebNSFWEnabledAt != nil || stored.WebBirthDateSetAt != nil || stored.WebTermsAcceptedAt != nil || stored.WebTermsAcceptedVersion != 0 || notifications.Load() != 1 {
						t.Fatalf("new identity inherited old facts: %v events=%d", err, notifications.Load())
					}
					var profile webAccountProfileModel
					if err := b.db.First(&profile, "account_id = ?", v.ID).Error; err != nil {
						t.Fatal(err)
					}
					if stored.Enabled || stored.WebTier != account.WebTierHeavy || stored.HealthRevision != v.HealthRevision || profile.EgressIdentity != "kept-egress" {
						t.Fatal("profile reset changed independently owned state")
					}
					if path == "import" {
						// Explicit export/import data remains an authorized source
						// of this newly installed material's completion facts.
						if err := apply(true); err != nil {
							t.Fatal(err)
						}
						stored, err = rb.Get(ctx, v.ID)
						at := now.Add(time.Hour)
						if err != nil || stored.WebNSFWEnabledAt == nil || !stored.WebNSFWEnabledAt.Equal(at) || stored.WebBirthDateSetAt == nil || !stored.WebBirthDateSetAt.Equal(at) || stored.WebTermsAcceptedAt == nil || !stored.WebTermsAcceptedAt.Equal(at) {
							t.Fatalf("explicit new material facts lost: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestPostgresWebProfileWaitHonorsCancellation(t *testing.T) {
	a, b := settingsDatabasePair(t, "postgres")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ra, rb := NewAccountRepository(a), NewAccountRepository(b)
	v := importFixture(t, ra, account.ProviderWeb, "profile-cancel", "")
	tx := a.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := lockProviderAccount(tx, v.ID, v.Provider); err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	event := account.WebProfileObservation{Kind: account.WebProfileNSFWEnabled, OccurredAt: time.Now()}
	go func() {
		result, err := rb.ApplyWebProfile(waitCtx, v.CredentialRef(), event)
		if result.Applied || result.Changed {
			done <- errors.New("canceled profile reported success")
			return
		}
		done <- err
	}()
	waitMediaDeletionLock(t, ctx, b, "accounts", done)
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled profile: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("profile waited after cancellation")
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	stored, err := ra.Get(ctx, v.ID)
	if err != nil || stored.WebNSFWEnabledAt != nil {
		t.Fatalf("canceled profile persisted: %v", err)
	}
	if result, err := rb.ApplyWebProfile(ctx, v.CredentialRef(), event); err != nil || !result.Applied || !result.Changed {
		t.Fatalf("canceled profile leaked resources: %+v %v", result, err)
	}
}
