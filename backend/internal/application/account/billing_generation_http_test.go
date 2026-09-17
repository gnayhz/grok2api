package account

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestBillingHTTPCompletionUsesObservedRecoveryGeneration(t *testing.T) {
	for _, scenario := range []string{"sync_reset", "sync_exhaustion", "sync_reimport", "probe_reset", "probe_exhaustion", "probe_failure_reset", "probe_cancel", "probe_cancel_reset", "probe_recovered"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "billing.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			encrypted, err := cipher.Encrypt("observed-access")
			if err != nil {
				t.Fatal(err)
			}
			v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "billing", SourceKey: "billing", EncryptedAccessToken: encrypted, ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			old := accountdomain.Billing{AccountID: v.ID, MonthlyLimit: 100, Used: 42, SyncedAt: now}
			probing := strings.HasPrefix(scenario, "probe_")
			if probing {
				old.Used = 100
				old.BillingPeriodEnd = now.Add(-time.Minute).Format(time.RFC3339)
			}
			seeded, err := repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), accountdomain.RecoveryEvent{Kind: accountdomain.RecoveryBillingObserved, Billing: &old, OccurredAt: now})
			if err != nil || !seeded.Applied {
				t.Fatal(err)
			}
			ref := seeded.Ref
			if probing {
				claimed, err := repo.ApplyQuotaRecovery(ctx, ref, accountdomain.RecoveryEvent{Kind: accountdomain.RecoveryProbeClaimed, OccurredAt: now})
				if err != nil || !claimed.Applied {
					t.Fatal(err)
				}
				ref = claimed.Ref
			}
			v, err = repo.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}, 1), make(chan struct{})
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/billing":
					calls.Add(1)
					if r.Header.Get("Authorization") != "Bearer observed-access" || r.URL.Query().Get("format") != "credits" {
						t.Error("Billing wire lost captured material or format")
					}
					started <- struct{}{}
					if strings.Contains(scenario, "cancel") {
						// Establish cancellation before any response exists.
						// Racing release against cancel can legitimately deliver
						// a known successful observation, a different contract.
						<-r.Context().Done()
						return
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					if scenario == "probe_failure_reset" {
						http.Error(w, "upstream failed", 503)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"monthlyLimit":100,"used":0,"billingPeriodEnd":%q}`, now.Add(time.Hour).Format(time.RFC3339))
				case "/v1/user":
					fmt.Fprint(w, `{"subscriptionTier":"SuperGrok"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			adapter := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
			s := NewService(repo, nil, nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			type completion struct {
				value     accountdomain.Credential
				recovered bool
				err       error
			}
			done := make(chan completion, 1)
			go func() {
				if probing {
					value, recovered, err := s.ProbePaidQuota(callCtx, v, ref)
					done <- completion{value, recovered, err}
				} else {
					_, err := s.RefreshBilling(callCtx, v.ID)
					done <- completion{err: err}
				}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("Billing HTTP did not start")
			}
			switch {
			case strings.HasSuffix(scenario, "reset"):
				if err := repo.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(scenario, "exhaustion"):
				result, err := repo.ApplyQuotaRecovery(ctx, ref, accountdomain.RecoveryEvent{Kind: accountdomain.RecoveryFreeExhausted, Used: 200, Limit: 200, OccurredAt: now})
				if err != nil || !result.Applied {
					t.Fatal(err)
				}
			case strings.HasSuffix(scenario, "reimport"):
				replacement := v
				replacement.EncryptedAccessToken = "replacement-encrypted"
				if _, _, err := repo.UpsertByIdentity(ctx, replacement); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(scenario, "cancel") {
				cancel()
			}
			close(release)
			var got completion
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Billing completion did not finish")
			}
			if calls.Load() != 1 {
				t.Fatalf("billing physical calls=%d", calls.Load())
			}
			billing, err := repo.GetBilling(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			recovery, recoveryErr := repo.GetQuotaRecovery(ctx, v.ID)
			if scenario == "probe_recovered" {
				if got.err != nil || !got.recovered || billing.Used != 0 || !errors.Is(recoveryErr, repository.ErrNotFound) || got.value.QuotaRecoveryRevision <= ref.Revision {
					t.Fatalf("promotion failed: %+v billing=%v recovery=%v", got, billing.Used, recoveryErr)
				}
				result, err := repo.ApplyQuotaRecovery(ctx, got.value.QuotaRecoveryRef(), accountdomain.RecoveryEvent{Kind: accountdomain.RecoveryFreeExhausted, Used: 1, Limit: 1})
				if err != nil || !result.Applied {
					t.Fatal("promoted request lost new revision")
				}
				return
			}
			if !probing && !errors.Is(got.err, ErrConflict) {
				t.Fatalf("obsolete synchronization must report a conflict: %v", got.err)
			}
			if got.err == nil || got.recovered || billing.Used != old.Used {
				t.Fatalf("old observation was published: recovered=%v err=%v used=%v", got.recovered, got.err, billing.Used)
			}
			switch {
			case strings.HasSuffix(scenario, "exhaustion"):
				if recoveryErr != nil || recovery.ConfirmedUsed != 200 {
					t.Fatalf("old completion erased new depletion: %+v %v", recovery, recoveryErr)
				}
			case scenario == "probe_cancel":
				if recoveryErr != nil || recovery.Status != accountdomain.QuotaRecoveryStatusExhausted || recovery.NextProbeAt == nil || recovery.NextProbeAt.Before(now.Add(14*time.Minute)) {
					t.Fatalf("canceled probe did not record bounded retry: %+v %v", recovery, recoveryErr)
				}
			default:
				if !errors.Is(recoveryErr, repository.ErrNotFound) {
					t.Fatalf("old completion recreated recovery: %+v %v", recovery, recoveryErr)
				}
			}
		})
	}
}
