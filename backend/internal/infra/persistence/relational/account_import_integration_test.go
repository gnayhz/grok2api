package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func importFixture(t *testing.T, repo *AccountRepository, provider account.Provider, key, email string) account.Credential {
	t.Helper()
	auth := account.AuthTypeSSO
	if provider == account.ProviderBuild {
		auth = account.AuthTypeOAuth
	}
	v, _, err := repo.UpsertByIdentity(context.Background(), account.Credential{Provider: provider, AuthType: auth, Name: key, SourceKey: key, Email: email, EncryptedAccessToken: "synthetic", Enabled: true, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAccountImportCurrentDeletionAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			for _, entry := range []string{"single", "batch", "linked", "status_cleanup"} {
				t.Run(entry, func(t *testing.T) {
					email := entry + "@example.test"
					v := importFixture(t, ra, account.ProviderWeb, entry, "  "+email+"  ")
					var err error
					switch entry {
					case "single":
						err = ra.Delete(ctx, v.ID)
					case "batch":
						_, err = ra.DeleteMany(ctx, []uint64{v.ID})
					case "linked":
						_, err = ra.DeleteManyWithLinked(ctx, v.Provider, []uint64{v.ID}, nil, false)
					case "status_cleanup":
						disabled := false
						if _, err := ra.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &disabled}}); err != nil {
							t.Fatal(err)
						}
						_, _, _, err = ra.DeleteAccountStatusBatchWithLinked(ctx, v.Provider, "disabled", time.Now(), 0, 500, nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					// A different provider and source must obey the same email intent.
					v.ID, v.Provider, v.SourceKey = 0, account.ProviderConsole, entry+"-console"
					out, err := rb.ImportAccounts(ctx, []repository.AccountImport{{Credential: v}})
					if err != nil || len(out) != 1 || out[0].Skipped != account.ImportTombstoned || out[0].ID != 0 || out[0].Created {
						t.Fatalf("current deletion bypassed: %+v, %v", out, err)
					}
					if _, _, err := rb.UpsertByIdentity(ctx, v); !errors.Is(err, repository.ErrConflict) {
						t.Fatalf("single convenience bypassed deletion: %v", err)
					}
					if n, err := rb.ClearTombstones(ctx, []string{"  " + email + "  "}); err != nil || n != 1 {
						t.Fatalf("explicit clear: %d, %v", n, err)
					}
					if _, created, err := rb.UpsertByIdentity(ctx, v); err != nil || !created {
						t.Fatalf("clear did not restore import: %t, %v", created, err)
					}
				})
			}
		})
	}
}

func TestAccountImportCurrentSourceAndTarget(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			for _, change := range []string{"source_deleted_unknown_email", "source_rotated", "source_email_blocked", "existing_target_email_blocked"} {
				t.Run(change, func(t *testing.T) {
					source := importFixture(t, ra, account.ProviderWeb, change, "")
					target := account.Credential{Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "target", SourceKey: change + "-target", EncryptedAccessToken: "late"}
					ref := source.CredentialRef()
					want := account.ImportSourceChanged
					var targetID uint64
					switch change {
					case "source_deleted_unknown_email":
						if err := rb.Delete(ctx, source.ID); err != nil {
							t.Fatal(err)
						}
					case "source_rotated":
						if _, _, err := rb.UpsertByIdentity(ctx, source); err != nil {
							t.Fatal(err)
						}
					case "source_email_blocked", "existing_target_email_blocked":
						want = account.ImportTombstoned
						email := change + "@example.test"
						deleted := importFixture(t, ra, account.ProviderBuild, change+"-deleted", email)
						if change == "source_email_blocked" {
							if _, err := rb.ApplyIdentity(ctx, source.CredentialRef(), account.IdentityObservation{Email: email}); err != nil {
								t.Fatal(err)
							}
						} else {
							stored := importFixture(t, ra, target.Provider, target.SourceKey, email)
							targetID = stored.ID
						}
						if err := rb.Delete(ctx, deleted.ID); err != nil {
							t.Fatal(err)
						}
					}
					out, err := ra.ImportAccounts(ctx, []repository.AccountImport{{Credential: target, Source: &ref}})
					if err != nil || len(out) != 1 || out[0].Skipped != want || out[0].ID != 0 {
						t.Fatalf("current identity ignored: %+v, %v", out, err)
					}
					if targetID != 0 {
						stored, err := rb.Get(ctx, targetID)
						if err != nil || stored.CredentialGeneration != 1 || stored.EncryptedAccessToken != "synthetic" {
							t.Fatalf("skipped target mutated: generation=%d, %v", stored.CredentialGeneration, err)
						}
					} else {
						var n int64
						if err := b.db.Model(&accountModel{}).Where("source_key = ?", target.SourceKey).Count(&n).Error; err != nil || n != 0 {
							t.Fatalf("skipped import persisted target: %d, %v", n, err)
						}
					}
				})
			}
		})
	}
}

func TestAccountDeletionTombstoneAtomicRollback(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, stage := range []string{"tombstone", "delete", "canceled_after_tombstone"} {
			t.Run(dialect+"/"+stage, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ra, rb := NewAccountRepository(a), NewAccountRepository(b)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				v := importFixture(t, ra, account.ProviderWeb, "rollback", "rollback@example.test")
				injected := errors.New("injected deletion transaction failure")
				if stage == "delete" {
					if err := a.db.Callback().Delete().Before("gorm:delete").Register("g20_deletion", func(tx *gorm.DB) {
						if tx.Statement.Table == "provider_accounts" {
							tx.AddError(injected)
						}
					}); err != nil {
						t.Fatal(err)
					}
				} else {
					callback := a.db.Callback().Create().Before("gorm:create")
					if stage == "canceled_after_tombstone" {
						callback = a.db.Callback().Create().After("gorm:create")
					}
					if err := callback.Register("g20_tombstone", func(tx *gorm.DB) {
						if tx.Statement.Table == "account_tombstones" {
							if stage == "tombstone" {
								tx.AddError(injected)
							} else {
								cancel()
							}
						}
					}); err != nil {
						t.Fatal(err)
					}
				}
				out, err := ra.DeleteManyWithLinked(ctx, v.Provider, []uint64{v.ID}, nil, false)
				if err == nil || out.Deleted != 0 {
					t.Fatalf("failed deletion reported success: %+v, %v", out, err)
				}
				if _, err := rb.Get(context.Background(), v.ID); err != nil {
					t.Fatalf("rollback lost account: %v", err)
				}
				marks, err := rb.TombstonedEmails(context.Background(), []string{v.Email})
				if err != nil || len(marks) != 0 {
					t.Fatalf("rollback retained deletion intent: %v, %v", marks, err)
				}
			})
		}
	}
}

func TestAccountImportDeletionRace(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			for round := range 12 {
				v := importFixture(t, ra, account.ProviderWeb, fmt.Sprint(round), fmt.Sprintf("race-%d@example.test", round))
				start := make(chan struct{})
				var wg sync.WaitGroup
				errs := make(chan error, 2)
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, err := ra.ImportAccounts(ctx, []repository.AccountImport{{Credential: v}})
					errs <- err
				}()
				go func() {
					defer wg.Done()
					<-start
					errs <- rb.Delete(ctx, v.ID)
				}()
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := ra.Get(ctx, v.ID); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("race resurrected account: %v", err)
				}
				marks, err := rb.TombstonedEmails(ctx, []string{v.Email})
				if err != nil || len(marks) != 1 {
					t.Fatalf("race lost current deletion: %v, %v", marks, err)
				}
			}
		})
	}
}

func TestAccountDeletionTombstonesFollowActualLinkedSet(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			r, other := NewAccountRepository(db), NewAccountRepository(peer)
			ctx := context.Background()
			var roots []uint64
			var emails []string
			for i, email := range []string{"root@example.test", "blocked-root@example.test", " SHARED@example.test "} {
				suffix := fmt.Sprint(i)
				digest := strings.Repeat(suffix, 64)
				web := importFixture(t, r, account.ProviderWeb, "sso:"+digest, email)
				buildEmail, consoleEmail := "build-"+suffix+"@example.test", "console-"+suffix+"@example.test"
				if i == 2 {
					buildEmail, consoleEmail = "shared@example.test", "Shared@example.test"
				}
				build := importFixture(t, r, account.ProviderBuild, "build-"+suffix, buildEmail)
				_ = importFixture(t, r, account.ProviderConsole, "console-sso:"+digest, consoleEmail)
				if err := r.LinkWebToBuild(ctx, web.CredentialRef(), build.CredentialRef()); err != nil {
					t.Fatal(err)
				}
				if err := r.ReconcileProviderLinks(ctx, web.ID); err != nil {
					t.Fatal(err)
				}
				roots = append(roots, web.ID)
				emails = append(emails, email, buildEmail, consoleEmail)
				if i == 1 {
					_, job := seedMediaDeletion(t, db, "g20", media.StatusInProgress)
					job.AccountID, job.AccountName = build.ID, build.Name
					if err := NewMediaJobRepository(db).CreateMediaJob(ctx, job); err != nil {
						t.Fatal(err)
					}
				}
			}
			out, err := r.DeleteManyWithLinked(ctx, account.ProviderWeb, roots, []account.Provider{account.ProviderBuild, account.ProviderConsole}, true)
			if err != nil || out.Deleted != 6 || out.RootsDeleted != 2 || len(out.SkippedRoots) != 1 || out.SkippedRoots[0] != roots[1] {
				t.Fatalf("linked deletion: %+v, %v", out, err)
			}
			marks, err := other.TombstonedEmails(ctx, emails)
			if err != nil || len(marks) != 4 {
				t.Fatalf("deleted roots/peers/deduplicated identity: %v, %v", marks, err)
			}
			for _, email := range emails[3:6] {
				if _, found := marks[account.ImportEmail(email)]; found {
					t.Fatal("media-protected group acquired tombstone")
				}
			}
		})
	}
}

func TestAccountAutoCleanAllowsReimport(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v := importFixture(t, ra, account.ProviderWeb, "auto", "auto@example.test")
			if _, err := ra.ApplyCredential(ctx, v.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRejected, OccurredAt: time.Now().Add(-2 * time.Hour), Reason: "synthetic"}); err != nil {
				t.Fatal(err)
			}
			ids, err := ra.DeleteAutoCleanReauthCandidates(ctx, time.Now().Add(-time.Hour), false, []uint64{v.ID})
			if err != nil || len(ids) != 1 {
				t.Fatalf("auto-clean: %v, %v", ids, err)
			}
			if _, created, err := rb.UpsertByIdentity(ctx, v); err != nil || !created {
				t.Fatalf("auto-clean changed explicit deletion policy: %t, %v", created, err)
			}
		})
	}
}

func TestAccountImportMaintenanceBarrier(t *testing.T) {
	for _, first := range []string{"import", "delete"} {
		t.Run(first, func(t *testing.T) {
			a, b := settingsDatabasePair(t, "postgres")
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			v := importFixture(t, ra, account.ProviderWeb, "barrier", "barrier@example.test")
			unrelated := importFixture(t, ra, account.ProviderWeb, "independent", "independent@example.test")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			reached, release := make(chan struct{}), make(chan struct{})
			var released sync.Once
			unblock := func() { released.Do(func() { close(release) }) }
			defer unblock()
			var paused atomic.Bool
			callback := func(tx *gorm.DB) {
				target := "account_credentials"
				if first == "delete" {
					target = "account_tombstones"
				}
				if tx.Statement.Table == target && paused.CompareAndSwap(false, true) {
					close(reached)
					select {
					case <-release:
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
			}
			if err := a.db.Callback().Create().Before("gorm:create").Register("g20_pause", callback); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Callback().Update().Before("gorm:update").Register("g20_pause_update", callback); err != nil {
				t.Fatal(err)
			}
			one, two := make(chan error, 1), make(chan error, 1)
			go func() {
				if first == "delete" {
					one <- ra.Delete(ctx, v.ID)
				} else {
					_, err := ra.ImportAccounts(ctx, []repository.AccountImport{{Credential: v}})
					one <- err
				}
			}()
			select {
			case <-reached:
			case err := <-one:
				t.Fatalf("operation did not reach barrier: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if first == "import" {
				// While the first import is held, another independent import
				// must finish: this barrier is not a global import mutex.
				parallelCtx, stop := context.WithTimeout(ctx, time.Second)
				_, err := rb.ImportAccounts(parallelCtx, []repository.AccountImport{{Credential: unrelated}})
				stop()
				if err != nil {
					t.Fatalf("unrelated imports serialized: %v", err)
				}
			}
			go func() {
				if first == "import" {
					two <- rb.Delete(ctx, v.ID)
				} else {
					out, err := rb.ImportAccounts(ctx, []repository.AccountImport{{Credential: v}})
					if err == nil && (len(out) != 1 || out[0].Skipped != account.ImportTombstoned) {
						err = fmt.Errorf("import ignored committed deletion: %+v", out)
					}
					two <- err
				}
			}()
			waitMediaDeletionLock(t, ctx, b, "pg_advisory_xact_lock", two)
			unblock()
			for _, result := range []<-chan error{one, two} {
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		})
	}
}

func TestAccountImportCancellationReleasesMaintenanceWait(t *testing.T) {
	a, b := settingsDatabasePair(t, "postgres")
	ra, rb := NewAccountRepository(a), NewAccountRepository(b)
	v := importFixture(t, ra, account.ProviderWeb, "canceled", "canceled@example.test")
	hold := a.db.Begin()
	if hold.Error != nil {
		t.Fatal(hold.Error)
	}
	defer hold.Rollback()
	if err := lockAccountLinkMutation(hold); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := rb.ImportAccounts(ctx, []repository.AccountImport{{Credential: v}})
		done <- err
	}()
	waitCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	waitMediaDeletionLock(t, waitCtx, b, "pg_advisory_xact_lock_shared", done)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled import: %v", err)
		}
	case <-waitCtx.Done():
		t.Fatal("canceled import retained its lock wait")
	}
	if err := hold.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	stored, err := rb.Get(context.Background(), v.ID)
	if err != nil || stored.CredentialGeneration != v.CredentialGeneration {
		t.Fatalf("canceled wait installed material: generation=%d, %v", stored.CredentialGeneration, err)
	}
	if err := rb.Delete(context.Background(), v.ID); err != nil {
		t.Fatalf("canceled import retained maintenance resources: %v", err)
	}
}

func TestAccountImportOppositeBatchOrders(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			left := importFixture(t, ra, account.ProviderWeb, "left", "left@example.test")
			right := importFixture(t, ra, account.ProviderWeb, "right", "right@example.test")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for range 3 {
				start, done := make(chan struct{}), make(chan error, 2)
				for i, repo := range []*AccountRepository{ra, rb} {
					inputs := []repository.AccountImport{{Credential: left}, {Credential: right}}
					if i == 1 {
						inputs[0], inputs[1] = inputs[1], inputs[0]
					}
					go func() {
						<-start
						out, err := repo.ImportAccounts(ctx, inputs)
						if err == nil && (len(out) != 2 || out[0].ID != inputs[0].Credential.ID || out[1].ID != inputs[1].Credential.ID) {
							err = fmt.Errorf("batch result lost input ordering: %+v", out)
						}
						done <- err
					}()
				}
				close(start)
				for range 2 {
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, original := range []account.Credential{left, right} {
				stored, err := rb.Get(ctx, original.ID)
				if err != nil || stored.CredentialGeneration != original.CredentialGeneration+6 {
					t.Fatalf("concurrent batches lost material generation: %d, %v", stored.CredentialGeneration, err)
				}
			}
		})
	}
}
