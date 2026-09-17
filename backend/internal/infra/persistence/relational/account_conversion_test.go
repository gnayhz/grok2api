package relational

import (
	"context"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAccountConversionMaterialAndLinkReferences(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			web := importFixture(t, ra, account.ProviderWeb, "conversion-web", "")
			build := importFixture(t, ra, account.ProviderBuild, "conversion-build", "")
			// Generation zero remains an exact legacy reference at both boundaries.
			if err := a.db.Model(&accountCredentialModel{}).Where("account_id IN ?", []uint64{web.ID, build.ID}).Update("generation", 0).Error; err != nil {
				t.Fatal(err)
			}
			web.CredentialGeneration, build.CredentialGeneration = 0, 0
			source, target := web.CredentialRef(), build.CredentialRef()
			input := repository.AccountImport{Credential: build, Source: &source, Target: &target}
			input.Credential.EncryptedAccessToken = "installed"
			results, err := ra.ImportAccounts(ctx, []repository.AccountImport{input})
			if err != nil || len(results) != 1 || results[0].Skipped != "" || results[0].Created || results[0].Material != (account.CredentialRef{AccountID: build.ID, Provider: account.ProviderBuild, Generation: 1}) {
				t.Fatalf("installed material reference: %+v %v", results, err)
			}
			installed := results[0].Material
			// A second importer is not allowed to substitute this old target snapshot.
			again, err := rb.ImportAccounts(ctx, []repository.AccountImport{input})
			if err != nil || len(again) != 1 || again[0].Skipped != account.ImportTargetChanged {
				t.Fatalf("old target overwrote material: %+v %v", again, err)
			}
			current, err := rb.Get(ctx, build.ID)
			if err != nil || current.EncryptedAccessToken != "installed" || current.CredentialRef() != installed {
				t.Fatalf("rejected target changed material: %v", err)
			}
			var notifications atomic.Int32
			ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { notifications.Add(1) })
			if err := ra.LinkWebToBuild(ctx, source, target); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("legacy target matched newer material: %v", err)
			}
			if notifications.Load() != 0 {
				t.Fatal("rejected link notified")
			}
			if err := ra.LinkWebToBuild(ctx, source, installed); err != nil {
				t.Fatal(err)
			}
			if err := ra.LinkWebToBuild(ctx, source, installed); err != nil {
				t.Fatalf("repeat current link: %v", err)
			}
			peer := importFixture(t, rb, account.ProviderWeb, "other-conversion-web", "")
			if err := ra.LinkWebToBuild(ctx, peer.CredentialRef(), installed); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("exclusive target stolen: %v", err)
			}
			replacement := web
			replacement.EncryptedAccessToken = "new-source"
			replaced, _, err := rb.UpsertByIdentity(ctx, replacement)
			if err != nil {
				t.Fatal(err)
			}
			if err := ra.LinkWebToBuild(ctx, source, installed); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("existing relation bypassed source generation: %v", err)
			}
			// The import receipt itself is stable even when later state changes.
			if results[0].Material != installed || replaced.CredentialGeneration != 1 {
				t.Fatal("committed references mutated")
			}
			if err := rb.Delete(ctx, build.ID); err != nil {
				t.Fatal(err)
			}
			if err := ra.LinkWebToBuild(ctx, replaced.CredentialRef(), installed); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("deleted target linked: %v", err)
			}
		})
	}
}

type conversionCommitAdapter struct {
	grant provider.CredentialSeed
	after func()
	calls int
}

func (*conversionCommitAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *conversionCommitAdapter) ConvertToBuild(context.Context, account.Credential) (provider.CredentialSeed, error) {
	a.calls++
	if a.after != nil {
		a.after()
	}
	return a.grant, nil
}

type conversionCommitPort struct {
	repository.AccountRepository
	after func(repository.AccountUpsertResult)
}

func (p *conversionCommitPort) ImportAccounts(ctx context.Context, inputs []repository.AccountImport) ([]repository.AccountUpsertResult, error) {
	out, err := p.AccountRepository.ImportAccounts(ctx, inputs)
	if err == nil && len(out) == 1 && out[0].Skipped == "" && p.after != nil {
		f := p.after
		p.after = nil
		f(out[0])
	}
	return out, err
}

func TestAccountConversionCompletionAcrossSQL(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			for _, scenario := range []string{"known_grant_cancel", "source_changed_before_install", "source_deleted_before_install", "source_changed_after_install", "target_changed_after_install", "linked_target_changed_during_provider", "linked_target_deleted_during_provider", "link_write_failed", "import_write_failed"} {
				t.Run(scenario, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					web := importFixture(t, ra, account.ProviderWeb, scenario+"-web", "")
					var target account.Credential
					if scenario == "linked_target_changed_during_provider" || scenario == "linked_target_deleted_during_provider" {
						target = importFixture(t, ra, account.ProviderBuild, scenario+"-build", "")
						if err := ra.LinkWebToBuild(ctx, web.CredentialRef(), target.CredentialRef()); err != nil {
							t.Fatal(err)
						}
					}
					grant := provider.CredentialSeed{Name: scenario, SourceKey: scenario + "-grant", AccessToken: "grant-access", RefreshToken: "grant-refresh", ExpiresAt: time.Now().Add(time.Hour)}
					adapter := &conversionCommitAdapter{grant: grant}
					port := &conversionCommitPort{AccountRepository: ra}
					injected := errors.New("conversion storage unavailable")
					replace := func(value account.Credential) {
						value.EncryptedAccessToken = "newer-material"
						if _, _, err := rb.UpsertByIdentity(context.Background(), value); err != nil {
							t.Fatal(err)
						}
					}
					switch scenario {
					case "known_grant_cancel":
						adapter.after = cancel
					case "source_changed_before_install":
						adapter.after = func() { replace(web) }
					case "source_deleted_before_install":
						adapter.after = func() {
							if err := rb.Delete(context.Background(), web.ID); err != nil {
								t.Fatal(err)
							}
						}
					case "linked_target_changed_during_provider":
						adapter.after = func() { replace(target) }
					case "linked_target_deleted_during_provider":
						adapter.after = func() {
							if err := rb.Delete(context.Background(), target.ID); err != nil {
								t.Fatal(err)
							}
						}
					case "source_changed_after_install":
						port.after = func(repository.AccountUpsertResult) { replace(web) }
					case "target_changed_after_install":
						port.after = func(result repository.AccountUpsertResult) {
							value, err := rb.Get(context.Background(), result.ID)
							if err != nil {
								t.Fatal(err)
							}
							replace(value)
						}
					case "link_write_failed", "import_write_failed":
						name := "g24_" + scenario
						if err := a.db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
							table := "account_provider_links"
							if scenario == "import_write_failed" {
								table = "account_credentials"
							}
							if tx.Statement.Table == table {
								tx.AddError(injected)
							}
						}); err != nil {
							t.Fatal(err)
						}
						defer a.db.Callback().Create().Remove(name)
					}
					svc := accountapp.NewService(port, NewAuditRepository(a), nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, memory.NewLockStore())
					observed := 0
					var progress [][2]int
					out, callErr := svc.ConvertWebAccountsToBuildWithStrategy(ctx, []uint64{web.ID}, accountapp.BuildConversionAll, func(uint64) error { observed++; return nil }, func(done, total int) error { progress = append(progress, [2]int{done, total}); return nil })
					if callErr != nil && !(scenario == "known_grant_cancel" && errors.Is(callErr, context.Canceled)) {
						t.Fatalf("batch control failed: %v", callErr)
					}
					if adapter.calls != 1 || !reflect.DeepEqual(progress, [][2]int{{0, 1}, {1, 1}}) {
						t.Fatalf("calls/progress: %d %v", adapter.calls, progress)
					}
					if scenario == "known_grant_cancel" {
						if out.Created != 1 || out.Failed != 0 || observed != 1 || len(out.BuildAccountIDs) != 1 {
							t.Fatalf("known canceled grant not completed: %+v observed=%d err=%v", out, observed, callErr)
						}
						saved, err := rb.Get(context.Background(), out.BuildAccountIDs[0])
						if err != nil {
							t.Fatal(err)
						}
						refresh, err := cipher.Decrypt(saved.EncryptedRefreshToken)
						if err != nil || refresh != "grant-refresh" || saved.LinkedAccountID != web.ID {
							t.Fatalf("saved grant/link: %v linked=%d", err, saved.LinkedAccountID)
						}
						return
					}
					skipped := scenario == "source_changed_before_install" || scenario == "source_deleted_before_install"
					if skipped {
						if out.Skipped != 1 || out.Failed != 0 {
							t.Fatalf("current source protection: %+v", out)
						}
					} else if out.Failed != 1 || out.Skipped != 0 {
						t.Fatalf("incomplete conversion reported success: %+v", out)
					}
					if out.Created != 0 || out.Linked != 0 || observed != 0 || len(out.BuildAccountIDs) != 0 {
						t.Fatalf("failed conversion exposed success: %+v observed=%d", out, observed)
					}
					var stored accountModel
					err = b.db.Where("provider = ? AND source_key = ?", account.ProviderBuild, grant.SourceKey).Take(&stored).Error
					retained := scenario == "source_changed_after_install" || scenario == "target_changed_after_install" || scenario == "link_write_failed"
					if retained {
						if err != nil {
							t.Fatalf("legally installed grant discarded: %v", err)
						}
						saved, err := rb.Get(context.Background(), stored.ID)
						if err != nil {
							t.Fatal(err)
						}
						if saved.LinkedAccountID != 0 {
							t.Fatalf("unconfirmed material linked: %d", saved.LinkedAccountID)
						}
						if scenario == "target_changed_after_install" && saved.EncryptedAccessToken != "newer-material" {
							t.Fatal("late completion overwrote newer material")
						}
					} else if !errors.Is(err, gorm.ErrRecordNotFound) {
						t.Fatalf("rejected installation changed destination: %v", err)
					}
					if scenario == "linked_target_changed_during_provider" {
						saved, err := rb.Get(context.Background(), target.ID)
						if err != nil || saved.EncryptedAccessToken != "newer-material" {
							t.Fatalf("old conversion replaced known newer target: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestPostgresConversionLinkWaitHonorsCancellation(t *testing.T) {
	a, b := settingsDatabasePair(t, "postgres")
	ra, rb := NewAccountRepository(a), NewAccountRepository(b)
	ctx := context.Background()
	web := importFixture(t, ra, account.ProviderWeb, "link-wait-web", "")
	build := importFixture(t, ra, account.ProviderBuild, "link-wait-build", "")
	tx := b.db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("SELECT id FROM provider_accounts WHERE id = ? FOR UPDATE", web.ID).Error; err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.LinkWebToBuild(waiting, web.CredentialRef(), build.CredentialRef()) }()
	waitMediaDeletionLock(t, ctx, b, "provider_accounts", done)
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lock cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("link wait did not cancel")
	}
	if time.Since(started) > time.Second {
		t.Fatal("link cancellation exceeded deadline")
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	if err := rb.LinkWebToBuild(ctx, web.CredentialRef(), build.CredentialRef()); err != nil {
		t.Fatal(fmt.Errorf("retry after canceled link: %w", err))
	}
}
