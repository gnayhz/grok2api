package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

// Real M07 writes, full routing projections and actual pinned acquisition must
// agree; enumeration itself must leave capacity and recovery state untouched.
func TestProbeCandidatesSharePinnedEligibility(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			var db *relational.Database
			var err error
			if dialect == "postgres" {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated TEST_POSTGRES_DSN required")
				}
				db, err = relational.OpenPostgres(ctx, dsn, 8, 4)
			} else {
				db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "candidates.db"))
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			models := relational.NewModelRepository(db)
			now := time.Now().UTC()
			for _, scenario := range []string{"ready", "disabled", "reauth", "risk", "cooling", "expired_cooling", "unknown_model", "unsupported_model", "model_access", "model_quota", "other_model_block", "expired_model_block", "billing", "window", "recovery_future", "recovery_due", "recovery_probing", "quality_held"} {
				t.Run(scenario, func(t *testing.T) {
					credential := account.Credential{Provider: account.ProviderBuild, Name: scenario, SourceKey: "probe-candidate-" + scenario + now.Format("150405.000000000"), Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "fixture", MaxConcurrent: 1}
					want := false
					switch scenario {
					case "ready", "expired_cooling", "unknown_model", "other_model_block", "expired_model_block", "quality_held":
						want = true
					case "disabled":
						credential.Enabled = false
					case "reauth":
						credential.AuthStatus = account.AuthStatusReauthRequired
					case "risk":
						credential.RiskStatus = "manual"
					case "cooling":
						future := now.Add(time.Hour)
						credential.CooldownUntil = &future
					}
					if scenario == "expired_cooling" {
						past := now.Add(-time.Hour)
						credential.CooldownUntil = &past
					}
					value, _, err := repo.UpsertByIdentity(ctx, credential)
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "disabled" {
						disabled := false
						if _, err := repo.UpdateAdministration(ctx, value.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &disabled}}); err != nil {
							t.Fatal(err)
						}
					}
					if scenario != "unknown_model" {
						supported := []string{"grok-4.6"}
						if scenario == "unsupported_model" {
							supported = []string{"grok-3"}
						}
						if err := testsupport.Capabilities(ctx, models, repo, value.ID, supported, now); err != nil {
							t.Fatal(err)
						}
					}
					switch scenario {
					case "model_access", "model_quota", "other_model_block", "expired_model_block":
						kind := account.ModelAccessDenied
						if scenario == "model_quota" {
							kind = account.ModelQuotaExhausted
						}
						event := account.ModelRestrictionEvent{Kind: kind, UpstreamModel: "grok-4.6", OccurredAt: now, RetryAfter: time.Hour}
						if scenario == "other_model_block" {
							event.UpstreamModel = "grok-3"
						}
						if scenario == "expired_model_block" {
							event.OccurredAt = now.Add(-2 * time.Hour)
						}
						if _, err := repo.ApplyModelRestriction(ctx, value.QuotaRecoveryRef(), event); err != nil {
							t.Fatal(err)
						}
					case "billing":
						billing := account.Billing{AccountID: value.ID, PlanCode: "pro", MonthlyLimit: 100, Used: 100, SyncedAt: now}
						if _, err := repo.ApplyQuotaRecovery(ctx, value.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryBillingObserved, Billing: &billing, OccurredAt: now}); err != nil {
							t.Fatal(err)
						}
					case "window":
						if err := repo.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: value.ID, SyncedAt: now, Windows: []account.QuotaWindow{{Mode: "fast", Remaining: 0, Source: account.QuotaSourceUpstream}}, ReplaceAll: true}); err != nil {
							t.Fatal(err)
						}
					case "recovery_future", "recovery_due", "recovery_probing":
						occurred := now
						if scenario != "recovery_future" {
							occurred = now.Add(-48 * time.Hour)
						}
						result, err := repo.ApplyQuotaRecovery(ctx, value.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: occurred})
						if err != nil {
							t.Fatal(err)
						}
						if scenario == "recovery_probing" {
							if _, err := repo.ApplyQuotaRecovery(ctx, result.Ref, account.RecoveryEvent{Kind: account.RecoveryProbeClaimed, OccurredAt: now}); err != nil {
								t.Fatal(err)
							}
						}
					}
					limiter := memory.NewConcurrencyLimiter()
					selector := NewSelector(repo, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
					if scenario == "quality_held" {
						selector.SetQualityEligibility(stubAccountEligibility{ineligible: map[uint64]bool{value.ID: true}})
					}
					before, err := repo.Get(ctx, value.ID)
					if err != nil {
						t.Fatal(err)
					}
					recoveryBefore, recoveryErr := repo.GetQuotaRecovery(ctx, value.ID)
					ids, err := selector.qualityProbeCandidates(ctx, account.ProviderBuild, 0, "grok-4.6", "fast")
					if err != nil {
						t.Fatal(err)
					}
					if got := slices.Contains(ids, value.ID); got != want {
						t.Fatalf("planned=%v want=%v", got, want)
					}
					current, err := limiter.Current(ctx, accountConcurrencyKey(value.ID))
					if err != nil || current != 0 {
						t.Fatalf("planning claimed capacity: %d %v", current, err)
					}
					lease, err := selector.AcquirePinnedForQualityProbe(ctx, account.ProviderBuild, value.ID, 0, "grok-4.6", "fast", clientkey.AccountScope{})
					if want {
						if err != nil || lease == nil {
							t.Fatalf("planned but not admitted: %v", err)
						}
						lease.Release()
					} else {
						if lease != nil {
							lease.Release()
							t.Fatal("ineligible account acquired a lease")
						}
						if err == nil {
							t.Fatal("ineligible account accepted")
						}
					}
					after, err := repo.Get(ctx, value.ID)
					if err != nil {
						t.Fatal(err)
					}
					recoveryAfter, afterErr := repo.GetQuotaRecovery(ctx, value.ID)
					if before.QuotaRecoveryRevision != after.QuotaRecoveryRevision || !reflect.DeepEqual(recoveryBefore, recoveryAfter) || errors.Is(recoveryErr, repository.ErrNotFound) != errors.Is(afterErr, repository.ErrNotFound) {
						t.Fatal("planning/measurement modified quota recovery")
					}
					if scenario == "recovery_due" {
						lease, err := selector.AcquirePinned(ctx, account.ProviderBuild, value.ID, 0, "grok-4.6", "fast", true)
						if err != nil || lease == nil {
							t.Fatalf("ordinary recovery stopped working: %v", err)
						}
						if !lease.QuotaProbe {
							t.Fatal("ordinary recovery did not claim probe")
						}
						lease.Release()
					}
				})
			}
			selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := selector.qualityProbeCandidates(cancelled, account.ProviderBuild, 0, "grok-4.6", "fast"); !errors.Is(err, context.Canceled) {
				t.Fatalf("candidate cancellation lost: %v", err)
			}
		})
	}
}
