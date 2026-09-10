package relational

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"gorm.io/gorm"
)

func TestRoutingClaimKeepsOneCommittedSnapshot(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			repo := NewAccountRepository(a)
			v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
				Name: "snapshot", SourceKey: "snapshot", Enabled: true, AuthStatus: account.AuthStatusActive,
				MaxConcurrent: 4, WebTier: account.WebTierBasic, EncryptedAccessToken: "synthetic-secret"})
			if err != nil {
				t.Fatal(err)
			}
			if err := saveQuotaWindowsFixture(repo, ctx, v.ID, account.WebTierBasic, time.Now(), []account.QuotaWindow{{AccountID: v.ID, Mode: "fast", Remaining: 20}}); err != nil {
				t.Fatal(err)
			}
			read, resume := make(chan struct{}), make(chan struct{})
			var once, resumeOnce sync.Once
			unblock := func() { resumeOnce.Do(func() { close(resume) }) }
			defer unblock()
			if err := a.db.Callback().Query().After("gorm:query").Register("routing_claim_snapshot", func(tx *gorm.DB) {
				if tx.Statement.Table == "provider_accounts" && tx.Error == nil {
					once.Do(func() {
						close(read)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					})
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer a.db.Callback().Query().Remove("routing_claim_snapshot")
			type result struct {
				candidate account.RoutingCandidate
				err       error
			}
			done := make(chan result, 1)
			go func() {
				c, err := repo.GetRoutingCandidate(ctx, v.ID, v.Provider, 0, "grok-test", "fast")
				done <- result{c, err}
			}()
			select {
			case <-read:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// Valid account operations share one outer transaction. No mixed
			// account/profile state is committed by this writer.
			writeErr := b.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				writer := NewAccountRepository(&Database{db: tx, dialect: dialect})
				limit := 1
				if _, err := writer.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{
					AccountUpdates: repository.AccountUpdates{MaxConcurrent: &limit},
					Risk:           &repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: "manual"},
				}); err != nil {
					return err
				}
				return saveQuotaWindowsFixture(writer, ctx, v.ID, account.WebTierSuper, time.Now(), []account.QuotaWindow{{AccountID: v.ID, Mode: "fast", Remaining: 80}})
			})
			unblock()
			actual := <-done
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			if actual.err != nil {
				t.Fatal(actual.err)
			}
			c := actual.candidate.Credential
			if c.RiskStatus != "" || c.MaxConcurrent != 4 || c.WebTier != account.WebTierBasic || actual.candidate.QuotaWindow == nil || actual.candidate.QuotaWindow.Remaining != 20 {
				t.Fatalf("mixed claim snapshot: risk=%s limit=%d tier=%s", c.RiskStatus, c.MaxConcurrent, c.WebTier)
			}
			current, err := repo.GetRoutingCandidate(ctx, v.ID, v.Provider, 0, "grok-test", "fast")
			if err != nil {
				t.Fatal(err)
			}
			c = current.Credential
			if c.RiskStatus != account.RiskStatusRSCDenied || c.MaxConcurrent != 1 || c.WebTier != account.WebTierSuper || current.QuotaWindow == nil || current.QuotaWindow.Remaining != 80 {
				t.Fatalf("next claim missed committed facts: risk=%s limit=%d tier=%s", c.RiskStatus, c.MaxConcurrent, c.WebTier)
			}
		})
	}
}

func TestRoutingClaimPreservesProjectionAndModelBinding(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, _ := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewAccountRepository(db)
			models := NewModelRepository(db)
			create := func(provider account.Provider, name string, tier account.WebTier, entitled bool) account.Credential {
				t.Helper()
				sourceKey := name
				if provider == account.ProviderWeb {
					sourceKey = "sso:" + strings.Repeat("a", 64)
				}
				v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: provider, AuthType: account.AuthTypeOAuth,
					Name: name, SourceKey: sourceKey, Enabled: true, AuthStatus: account.AuthStatusActive, WebTier: tier,
					BuildSuperEntitled: entitled, EncryptedAccessToken: "access-secret", EncryptedRefreshToken: "refresh-secret", EncryptedCloudflareCookie: "cookie-secret"})
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			web := create(account.ProviderWeb, "web", account.WebTierBasic, false)
			free := create(account.ProviderBuild, "free", "", false)
			super := create(account.ProviderBuild, "super", "", true)
			source := create(account.ProviderBuild, "source", "", true)
			console := create(account.ProviderConsole, "console", "", false)
			if err := repo.LinkWebToBuild(ctx, web.CredentialRef(), super.CredentialRef()); err != nil {
				t.Fatal(err)
			}
			for _, v := range []account.Credential{free, super, source} {
				capabilities := []string{"other"}
				if v.ID == source.ID {
					capabilities = []string{"shared-model"}
				}
				if err := testsupport.Capabilities(ctx, models, repo, v.ID, capabilities, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveQuotaWindowsFixture(repo, ctx, web.ID, account.WebTierBasic, time.Now(), []account.QuotaWindow{
				{AccountID: web.ID, Mode: "weekly", Remaining: 90}, {AccountID: web.ID, Mode: account.QuotaModeWebImagePro, Remaining: 20},
				{AccountID: web.ID, Mode: account.QuotaModeWebImageEdit, Remaining: 5},
			}); err != nil {
				t.Fatal(err)
			}
			for _, query := range []struct {
				provider    account.Provider
				model, mode string
			}{
				{account.ProviderBuild, "shared-model", ""}, {account.ProviderWeb, "image-model", account.QuotaModeWebImageEdit}, {account.ProviderConsole, "static-console", "console"},
			} {
				values, err := repo.ListRoutingCandidates(ctx, query.provider, 0, query.model, query.mode)
				if err != nil {
					t.Fatal(err)
				}
				for _, expected := range values {
					got, err := repo.GetRoutingCandidate(ctx, expected.Credential.ID, query.provider, 0, query.model, query.mode)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, expected) {
						t.Fatalf("selected projection differs for account %d", expected.Credential.ID)
					}
					if got.Credential.EncryptedAccessToken != "" || got.Credential.EncryptedRefreshToken != "" || got.Credential.EncryptedCloudflareCookie != "" {
						t.Fatal("routing claim loaded secrets")
					}
				}
			}
			shared, err := repo.GetRoutingCandidate(ctx, super.ID, super.Provider, 0, "shared-model", "")
			if err != nil || !shared.SupportsModel || !shared.ModelCapabilityKnown || shared.Credential.EgressIdentity == "" {
				t.Fatalf("shared model/link missing: support=%t known=%t entitled=%t identity=%q err=%v", shared.SupportsModel, shared.ModelCapabilityKnown, shared.Credential.BuildSuperEntitled, shared.Credential.EgressIdentity, err)
			}
			unpaid, err := repo.GetRoutingCandidate(ctx, free.ID, free.Provider, 0, "shared-model", "")
			if err != nil || unpaid.SupportsModel || !unpaid.ModelCapabilityKnown {
				t.Fatalf("free inherited Super catalog: err=%v", err)
			}
			route, err := models.Create(ctx, model.Route{Provider: account.ProviderBuild, PublicID: "binding-test", UpstreamModel: "shared-model", Capability: model.CapabilityChat, Enabled: true}, []uint64{free.ID})
			if err != nil {
				t.Fatal(err)
			}
			for _, routeID := range []uint64{0, route.ID} {
				bound, err := repo.GetRoutingCandidate(ctx, free.ID, free.Provider, routeID, "shared-model", "")
				if err != nil || !bound.SupportsModel {
					t.Fatalf("binding did not supply capability: %v", err)
				}
				if _, err := repo.GetRoutingCandidate(ctx, super.ID, super.Provider, routeID, "shared-model", ""); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("unbound account admitted: %v", err)
				}
			}
			for _, id := range []uint64{0, console.ID, 999999} {
				if _, err := repo.GetRoutingCandidate(ctx, id, account.ProviderWeb, 0, "grok-test", ""); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("invalid identity %d: %v", id, err)
				}
			}
			// The next selected read must see an authentication restriction even
			// when a previous provider candidate slice is still in use.
			if _, err := repo.ApplyCredential(ctx, web.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRejected, Reason: "restricted"}); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.GetRoutingCandidate(ctx, web.ID, web.Provider, 0, "grok-test", ""); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("auth restriction missed: %v", err)
			}
		})
	}
}

func TestRoutingClaimFailureNeverReturnsPartialFacts(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, _ := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewAccountRepository(db)
			v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "partial", SourceKey: "partial", EncryptedAccessToken: "synthetic", Enabled: true, AuthStatus: account.AuthStatusActive, WebTier: account.WebTierBasic})
			if err != nil {
				t.Fatal(err)
			}
			for _, canceled := range []bool{false, true} {
				t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
					readCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					failure := errors.New("routing window unavailable")
					callback := "routing_claim_failure"
					if err := db.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Table == "quota" {
							if canceled {
								cancel()
							} else {
								tx.AddError(failure)
							}
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer db.db.Callback().Query().Remove(callback)
					c, err := repo.GetRoutingCandidate(readCtx, v.ID, v.Provider, 0, "grok-test", "")
					want := error(failure)
					if canceled {
						want = context.Canceled
					}
					if !errors.Is(err, want) || c.Credential.ID != 0 {
						t.Fatalf("partial facts escaped: id=%d err=%v", c.Credential.ID, err)
					}
				})
			}
			if _, err := repo.GetRoutingCandidate(ctx, v.ID, v.Provider, 0, "grok-test", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}
