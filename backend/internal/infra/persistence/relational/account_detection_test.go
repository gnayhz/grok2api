package relational

import (
	"context"
	"errors"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type detectionRefusalAdapter struct {
	before func()
	closed atomic.Bool
}

func (*detectionRefusalAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *detectionRefusalAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	if a.before != nil {
		a.before()
	}
	return &provider.Response{StatusCode: http.StatusUnauthorized, Body: detectionRefusalBody{Reader: strings.NewReader(`{"error":{"code":"invalid_api_key"}}`), closed: &a.closed}}, nil
}

type detectionRefusalBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b detectionRefusalBody) Close() error { b.closed.Store(true); return nil }

func TestAccountDetectionCurrentCommitAcrossSQL(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			encrypted, err := cipher.Encrypt("synthetic")
			if err != nil {
				t.Fatal(err)
			}
			for _, scenario := range []string{"current", "legacy_zero", "replacement_before", "replacement_after", "deletion_before", "deletion_after", "write_failure", "known_refusal_cancel"} {
				t.Run(scenario, func(t *testing.T) {
					ctx := context.Background()
					callCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, SourceKey: scenario, Name: scenario,
						EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), RefreshPermanent: true, AuthStatus: account.AuthStatusActive})
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "legacy_zero" {
						if err := a.db.Model(&accountCredentialModel{}).Where("account_id = ?", v.ID).Update("generation", 0).Error; err != nil {
							t.Fatal(err)
						}
						v.CredentialGeneration = 0
					}
					adapter := &detectionRefusalAdapter{}
					mutate := func() {
						if strings.HasPrefix(scenario, "deletion") {
							if err := rb.Delete(ctx, v.ID); err != nil {
								t.Error(err)
							}
						} else {
							if _, _, err := rb.UpsertByIdentity(ctx, v); err != nil {
								t.Error(err)
							}
						}
					}
					if strings.HasSuffix(scenario, "_before") {
						adapter.before = mutate
					}
					var events atomic.Int32
					ra.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) {
						if e.Kind == repository.InvalidationAccountCredentialChanged && events.Add(1) == 1 && strings.HasSuffix(scenario, "_after") {
							mutate()
						}
					})
					defer ra.SetInvalidationObserver(nil)
					if scenario == "write_failure" {
						name := "test:detection-state-failure"
						if err := a.db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
							if tx.Statement.Table == "provider_accounts" {
								tx.AddError(errors.New("detection state write failed"))
							}
						}); err != nil {
							t.Fatal(err)
						}
						defer a.db.Callback().Update().Remove(name)
					}
					if scenario == "known_refusal_cancel" {
						adapter.before = cancel
					}
					service := accountapp.NewService(ra, nil, nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
					var items []accountapp.BuildDetectItemResult
					succeeded, failed, callErr := service.DetectBuildAccountsWithProgress(callCtx, []uint64{v.ID}, false, nil, func(item accountapp.BuildDetectItemResult) error { items = append(items, item); return nil })
					if succeeded != 0 || failed != 1 || len(items) != 1 || !adapter.closed.Load() {
						t.Fatalf("counts=%d/%d items=%d closed=%v", succeeded, failed, len(items), adapter.closed.Load())
					}
					if scenario == "known_refusal_cancel" {
						if !errors.Is(callErr, context.Canceled) {
							t.Fatalf("cancel error=%v", callErr)
						}
					} else if callErr != nil {
						t.Fatal(callErr)
					}
					wantOutcome := accountapp.BuildDetectOutcomeFailed
					wantAuth := account.AuthStatusActive
					if scenario == "current" || scenario == "legacy_zero" || scenario == "known_refusal_cancel" {
						wantOutcome, wantAuth = accountapp.BuildDetectOutcomeInvalid, account.AuthStatusReauthRequired
					}
					if items[0].Outcome != wantOutcome {
						t.Fatalf("item=%+v want=%s", items[0], wantOutcome)
					}
					current, err := rb.Get(ctx, v.ID)
					if strings.HasPrefix(scenario, "deletion") {
						if !errors.Is(err, repository.ErrNotFound) {
							t.Fatalf("deleted account restored: %v", err)
						}
						return
					}
					if err != nil || current.AuthStatus != wantAuth || current.HealthRevision != v.HealthRevision {
						t.Fatalf("auth/health=%s/%d err=%v", current.AuthStatus, current.HealthRevision, err)
					}
					wantGeneration := v.CredentialGeneration
					if strings.HasPrefix(scenario, "replacement") {
						wantGeneration++
					}
					if current.CredentialGeneration != wantGeneration {
						t.Fatalf("generation=%d want=%d", current.CredentialGeneration, wantGeneration)
					}
					if scenario == "write_failure" && (events.Load() != 0 || !strings.Contains(items[0].Reason, "detection state write failed")) {
						t.Fatalf("failed commit emitted success/notification: %+v %d", items[0], events.Load())
					}
				})
			}
		})
	}
}
