package relational

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAccountIdentityObservationUsesCurrentMaterial(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			for _, kind := range []account.Provider{account.ProviderWeb, account.ProviderConsole} {
				t.Run(string(kind), func(t *testing.T) {
					v := importFixture(t, ra, kind, string(kind), string(kind)+"-kept@example.test")
					// Legacy generation zero is an exact identity, never a wildcard.
					if err := a.db.Model(&accountCredentialModel{}).Where("account_id = ?", v.ID).Update("generation", 0).Error; err != nil {
						t.Fatal(err)
					}
					v.CredentialGeneration = 0
					events := 0
					ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { events++ })
					defer ra.SetInvalidationObserver(nil)
					identity := account.IdentityObservation{UserID: "11111111-1111-4111-8111-111111111111", TeamID: " team "}
					out, err := ra.ApplyIdentity(ctx, v.CredentialRef(), identity)
					if err != nil || !out.Applied || out.State.Identity.Email != v.Email || out.State.Identity.TeamID != "team" || events != 1 {
						t.Fatalf("current legacy observation: %+v %v events=%d", out, err, events)
					}
					replacement := v
					replacement.Email, replacement.UserID, replacement.TeamID = string(kind)+"-fresh@example.test", "22222222-2222-4222-8222-222222222222", "fresh"
					replacement.EncryptedAccessToken = "replacement"
					fresh, _, err := rb.UpsertByIdentity(ctx, replacement)
					if err != nil {
						t.Fatal(err)
					}
					out, err = ra.ApplyIdentity(ctx, v.CredentialRef(), identity)
					if err != nil || out.Applied || out.State.Material != fresh.CredentialRef() || out.State.Identity.Email != replacement.Email || events != 1 {
						t.Fatalf("stale result: %+v %v events=%d", out, err, events)
					}
					stored, err := rb.Get(ctx, v.ID)
					if err != nil || stored.CredentialRef() != fresh.CredentialRef() || stored.EncryptedAccessToken != "replacement" || stored.UserID != fresh.UserID || stored.TeamID != fresh.TeamID || stored.Enabled != fresh.Enabled || stored.HealthRevision != fresh.HealthRevision {
						t.Fatalf("stale identity changed current material or independent state: %v", err)
					}
					wrong := fresh.CredentialRef()
					wrong.Provider = account.ProviderBuild
					if _, err := ra.ApplyIdentity(ctx, wrong, identity); !errors.Is(err, repository.ErrNotFound) {
						t.Fatalf("wrong provider: %v", err)
					}
					if _, err := ra.ApplyIdentity(ctx, fresh.CredentialRef(), account.IdentityObservation{Email: strings.Repeat("x", 256)}); err == nil {
						t.Fatal("oversized identity accepted")
					}
					if err := rb.Delete(ctx, v.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := ra.ApplyIdentity(ctx, fresh.CredentialRef(), identity); !errors.Is(err, repository.ErrNotFound) {
						t.Fatalf("deleted account: %v", err)
					}
					if events != 1 {
						t.Fatalf("rejected observations notified: %d", events)
					}
				})
			}
		})
	}
}

func TestIdentityObservationProjectsOnlyCommittedCurrentLinks(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b, db := qualityManagementPair(t, dialect)
			ctx := context.Background()
			repo := NewAccountRepository(db)
			web := importFixture(t, repo, account.ProviderWeb, "web", "")
			oldBuild := importFixture(t, repo, account.ProviderBuild, "old-build", "")
			newBuild := importFixture(t, repo, account.ProviderBuild, "new-build", "")
			oldUID, newUID := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
			for id, uid := range map[uint64]string{oldBuild.ID: oldUID, newBuild.ID: newUID} {
				if err := db.db.Model(&accountModel{}).Where("id = ?", id).Update("user_id", uid).Error; err != nil {
					t.Fatal(err)
				}
			}
			replacement := web
			replacement.EncryptedAccessToken = "new"
			fresh, _, err := repo.UpsertByIdentity(ctx, replacement)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := repo.ApplyIdentity(ctx, web.CredentialRef(), account.IdentityObservation{UserID: oldUID}); err != nil || out.Applied {
				t.Fatalf("stale apply: %+v %v", out, err)
			}
			if err := a.RefreshIdentityGroups(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.RefreshState(ctx); err != nil {
				t.Fatal(err)
			}
			if _, members := b.IdentityGroupOf(web.ID); !slices.Equal(members, []uint64{web.ID}) {
				t.Fatalf("old observation created a quality group: %v", members)
			}
			if out, err := repo.ApplyIdentity(ctx, fresh.CredentialRef(), account.IdentityObservation{UserID: newUID}); err != nil || !out.Applied {
				t.Fatalf("current apply: %+v %v", out, err)
			}
			if err := a.RefreshIdentityGroups(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.RefreshState(ctx); err != nil {
				t.Fatal(err)
			}
			if _, members := b.IdentityGroupOf(web.ID); !slices.Equal(members, []uint64{web.ID, newBuild.ID}) {
				t.Fatalf("current association missing from shared quality projection: %v", members)
			}
			if _, members := b.IdentityGroupOf(oldBuild.ID); !slices.Equal(members, []uint64{oldBuild.ID}) {
				t.Fatalf("old Build joined new identity: %v", members)
			}
			candidate, err := repo.GetRoutingCandidate(ctx, web.ID, web.Provider, 0, "grok-test", "")
			if err != nil || candidate.Credential.UserID != newUID || candidate.Credential.EncryptedAccessToken != "" {
				t.Fatalf("routing claim lost current identity: %v", err)
			}
		})
	}
}

func TestPostgresIdentityObservationWaitHonorsCancellation(t *testing.T) {
	a, b := settingsDatabasePair(t, "postgres")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ra, rb := NewAccountRepository(a), NewAccountRepository(b)
	v := importFixture(t, ra, account.ProviderWeb, "cancel", "")
	tx := a.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := lockAccountImport(tx); err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		out, err := rb.ApplyIdentity(waitCtx, v.CredentialRef(), account.IdentityObservation{Email: "changed@example.test"})
		if out.Applied {
			done <- errors.New("canceled identity reported applied")
			return
		}
		done <- err
	}()
	waitMediaDeletionLock(t, ctx, b, "pg_advisory_xact_lock", done)
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("identity stayed blocked after cancellation")
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	stored, err := ra.Get(ctx, v.ID)
	if err != nil || stored.Email != "" {
		t.Fatalf("canceled observation changed identity: %v", err)
	}
	if out, err := rb.ApplyIdentity(ctx, v.CredentialRef(), account.IdentityObservation{Email: "retry@example.test"}); err != nil || !out.Applied {
		t.Fatalf("canceled transaction leaked lock: %+v %v", out, err)
	}
}

func TestAccountIdentityAndLinksCommitTogether(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v := importFixture(t, ra, account.ProviderWeb, "web", "before@example.test")
			build := importFixture(t, ra, account.ProviderBuild, "build", "build@example.test")
			uid := "11111111-1111-4111-8111-111111111111"
			if err := a.db.Model(&accountModel{}).Where("id = ?", build.ID).Update("user_id", uid).Error; err != nil {
				t.Fatal(err)
			}
			identity := account.IdentityObservation{Email: "after@example.test", UserID: uid, TeamID: "team"}
			events := 0
			ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { events++ })
			fault := errors.New("injected link failure")
			name := "g21_identity_link_failure"
			if err := a.db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
				if tx.Statement.Table == "account_provider_links" {
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			out, err := ra.ApplyIdentity(ctx, v.CredentialRef(), identity)
			if removeErr := a.db.Callback().Create().Remove(name); removeErr != nil {
				t.Fatal(removeErr)
			}
			if !errors.Is(err, fault) || out.Applied || events != 0 {
				t.Fatalf("link failure leaked commit: %+v %v events=%d", out, err, events)
			}
			stored, err := rb.Get(ctx, v.ID)
			if err != nil || stored.Email != v.Email || stored.UserID != "" || stored.LinkedAccountID != 0 {
				t.Fatalf("identity survived failed link: %v", err)
			}
			out, err = ra.ApplyIdentity(ctx, v.CredentialRef(), identity)
			if err != nil || !out.Applied || events != 1 {
				t.Fatalf("retry failed: %+v %v", out, err)
			}
			stored, err = rb.Get(ctx, v.ID)
			if err != nil || stored.Email != identity.Email || stored.LinkedAccountID != build.ID {
				t.Fatalf("identity and link not visible together: %v", err)
			}
		})
	}
}

func TestAccountIdentityConcurrentReplacementKeepsNewFacts(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			for i := 0; i < 8; i++ {
				v := importFixture(t, ra, account.ProviderWeb, fmt.Sprint(i), "")
				replacement := v
				replacement.Email, replacement.UserID, replacement.TeamID, replacement.EncryptedAccessToken = fmt.Sprintf("fresh-%d@example.test", i), fmt.Sprintf("22222222-2222-4222-8222-%012d", i), "fresh", "new"
				start, done := make(chan struct{}), make(chan error, 2)
				go func() {
					<-start
					_, err := ra.ApplyIdentity(ctx, v.CredentialRef(), account.IdentityObservation{Email: "old@example.test", UserID: fmt.Sprintf("11111111-1111-4111-8111-%012d", i)})
					done <- err
				}()
				go func() { <-start; _, _, err := rb.UpsertByIdentity(ctx, replacement); done <- err }()
				close(start)
				for j := 0; j < 2; j++ {
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				stored, err := rb.Get(ctx, v.ID)
				if err != nil || stored.CredentialGeneration != v.CredentialGeneration+1 || stored.Email != replacement.Email || stored.UserID != replacement.UserID || stored.TeamID != replacement.TeamID {
					t.Fatalf("old observation beat replacement: %v", err)
				}
			}
		})
	}
}
