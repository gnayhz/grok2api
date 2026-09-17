package selector

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func currentFactsAccount(t *testing.T, repo *relational.AccountRepository, provider account.Provider) (account.Credential, map[uint64]bool) {
	t.Helper()
	ctx := context.Background()
	existing, err := repo.ListRoutingCandidates(ctx, provider, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	excluded := make(map[uint64]bool)
	for _, v := range existing {
		excluded[v.Credential.ID] = true
	}
	name := fmt.Sprintf("g19-%s-%d", t.Name(), time.Now().UnixNano())
	v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: provider, AuthType: account.AuthTypeOAuth,
		Name: name, SourceKey: name, WebTier: account.WebTierBasic, ObservedModel: "grok-build-free",
		Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "synthetic", MaxConcurrent: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Delete(ctx, v.ID); err != nil {
			t.Error(err)
		}
	})
	return v, excluded
}

func TestSelectorCurrentHardFactsKeepRefusalReasons(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			for _, path := range []string{"session", "pinned", "quality_probe"} {
				for _, restriction := range []string{"disabled", "auth", "cooldown", "model_access", "model_capability", "quota_window", "build_entitlement", "build_bot_flag", "minimum_remaining", "recovery"} {
					t.Run(path+"/"+restriction, func(t *testing.T) {
						ctx := context.Background()
						provider := account.ProviderBuild
						mode := ""
						if restriction == "quota_window" {
							provider, mode = account.ProviderWeb, "fast"
						}
						v, excluded := currentFactsAccount(t, repo, provider)
						port := &currentClaimRepository{AccountRepository: repo}
						limiter := memory.NewConcurrencyLimiter()
						selector := NewSelector(port, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
						selector.UpdateExcludeBuildBotFlaggedFromScheduling(true)
						scope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeAll, Tiers: clientkeydomain.TierScopeAll}
						if restriction == "build_entitlement" {
							scope.Tiers = clientkeydomain.TierScopeFree
						}
						model := fmt.Sprintf("g19-model-%d", v.ID)
						if restriction == "minimum_remaining" {
							_, err := repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryBillingObserved,
								Billing: &account.Billing{AccountID: v.ID, MonthlyLimit: 10, Used: 9, SyncedAt: time.Now()}})
							if err != nil {
								t.Fatal(err)
							}
							var errGet error
							v, errGet = repo.Get(ctx, v.ID)
							if errGet != nil {
								t.Fatal(errGet)
							}
						}
						var session *selectionSession
						var err error
						if path == "session" {
							session, err = selector.beginSelectionSessionForKey(ctx, provider, 0, model, mode, "", excluded, false, scope)
							if err != nil {
								t.Fatal(err)
							}
						}
						want := SelectionNoAccounts
						port.before = func(id uint64) error {
							switch restriction {
							case "disabled":
								enabled := false
								_, err = repo.UpdateAdministration(ctx, id, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &enabled}})
							case "auth":
								_, err = repo.ApplyCredential(ctx, v.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRejected, Reason: "g19 rejected"})
							case "cooldown":
								want = SelectionCooling
								_, err = repo.ApplyHealth(ctx, id, provider, account.HealthEvent{Kind: account.HealthFailure, Status: 429, RetryAfter: time.Hour})
							case "model_access":
								want = SelectionModelCooling
								_, err = repo.ApplyModelRestriction(ctx, v.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: account.ModelAccessDenied, UpstreamModel: model, RetryAfter: time.Hour})
							case "model_capability":
								want = SelectionUnsupportedModel
								err = testsupport.Capabilities(ctx, relational.NewModelRepository(db), repo, id, []string{"some-other-model"}, time.Now())
							case "quota_window":
								want = SelectionQuotaExhausted
								err = saveQuotaWindowsFixture(repo, ctx, id, account.WebTierBasic, time.Now(), []account.QuotaWindow{{AccountID: id, Mode: mode, Remaining: 0, Total: 100, Source: account.QuotaSourceUpstream}})
							case "build_entitlement":
								entitled := true
								_, err = repo.UpdateAdministration(ctx, id, repository.AccountAdminPatch{BuildSuperEntitled: &entitled})
							case "build_bot_flag":
								_, err = repo.ApplyCredential(ctx, v.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRefreshed, AccessToken: "rotated", BuildBotFlagSource: 2, ExpiresAt: time.Now().Add(time.Hour)})
							case "minimum_remaining":
								want = SelectionQuotaExhausted
								minimum := 2.0
								_, err = repo.UpdateAdministration(ctx, id, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{MinimumRemaining: &minimum}})
							case "recovery":
								want = SelectionQuotaExhausted
								_, err = repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: time.Now()})
							}
							return err
						}
						var lease *accountLease
						if session != nil {
							lease, err = session.Acquire(ctx, excluded, false)
						} else if path == "quality_probe" {
							lease, err = selector.AcquirePinnedForQualityProbe(ctx, provider, v.ID, 0, model, mode, scope)
						} else {
							lease, err = selector.AcquirePinnedForKey(ctx, provider, v.ID, 0, model, mode, true, scope)
						}
						if lease != nil {
							lease.Release()
							t.Error("committed hard restriction admitted")
						}
						var unavailable *SelectionUnavailableError
						if !errors.As(err, &unavailable) || unavailable.Reason != want {
							t.Errorf("refusal=%v, want %s", err, want)
						}
						if port.reads != 1 || port.materials != 0 {
							t.Errorf("reads=%d material=%d", port.reads, port.materials)
						}
						assertSelectorCapacityReleased(t, limiter, []account.Credential{v})
					})
				}
			}
		})
	}
}

func TestSelectorClaimsWithCurrentConcurrentLimit(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			for _, runtime := range []string{"memory", "redis"} {
				for _, path := range []string{"ordinary", "session", "pinned", "sticky"} {
					t.Run(runtime+"/"+path, func(t *testing.T) {
						ctx := context.Background()
						v, excluded := currentFactsAccount(t, repo, account.ProviderBuild)
						limiter := selectorCapacityLimiter(t, runtime)
						release, ok, err := limiter.Acquire(ctx, repository.AccountConcurrencyKey(v.ID), 3)
						if err != nil || !ok {
							t.Fatalf("seed actual capacity: %t %v", ok, err)
						}
						defer release()
						port := &currentClaimRepository{AccountRepository: repo}
						sticky := memory.NewStickyStore()
						selector := NewSelector(port, limiter, sticky, nil, time.Hour, time.Second, time.Minute)
						affinity := ""
						if path == "sticky" {
							affinity = "g19-current-limit"
							if err := sticky.Set(ctx, stickySessionKey(affinity), v.ID, time.Now().Add(time.Hour)); err != nil {
								t.Fatal(err)
							}
						}
						var session *selectionSession
						if path == "session" {
							session, err = selector.beginSelectionSession(ctx, v.Provider, 0, "grok-test", "", "", excluded, false)
							if err != nil {
								t.Fatal(err)
							}
						}
						port.before = func(id uint64) error {
							limit := 1
							_, err := repo.UpdateAdministration(ctx, id, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{MaxConcurrent: &limit}})
							return err
						}
						var lease *accountLease
						if session != nil {
							lease, err = session.Acquire(ctx, excluded, false)
						} else if path == "pinned" {
							lease, err = selector.AcquirePinnedForKey(ctx, v.Provider, v.ID, 0, "grok-test", "", true, clientkeydomain.AccountScope{})
						} else {
							if s, serr := selector.beginSelectionSession(ctx, v.Provider, 0, "grok-test", "", affinity, excluded, false); serr != nil {
								lease, err = nil, serr
							} else {
								lease, err = s.Acquire(ctx, excluded, false)
							}
						}
						if lease != nil {
							lease.Release()
							t.Error("claimed above current limit")
						}
						if !isSelectionUnavailable(err, SelectionSaturated) {
							t.Errorf("limit refusal=%v", err)
						}
						if port.materials != 0 {
							t.Error("saturated candidate loaded material")
						}
						if n, err := limiter.Current(ctx, repository.AccountConcurrencyKey(v.ID)); err != nil || n != 1 {
							t.Errorf("actual capacity=%d err=%v", n, err)
						}
						release()
						if s, serr := selector.beginSelectionSession(ctx, v.Provider, 0, "grok-test", "", "", excluded, false); serr != nil {
							lease, err = nil, serr
						} else {
							lease, err = s.Acquire(ctx, excluded, false)
						}
						if err != nil || lease == nil {
							t.Fatalf("released capacity unavailable: %v", err)
						}
						if lease.Credential.MaxConcurrent != 1 {
							t.Error("lease retained old concurrency metadata")
						}
						lease.Release()
						assertSelectorCapacityReleased(t, limiter, []account.Credential{v})
					})
				}
			}
		})
	}
}

func TestSelectionSessionUsesCurrentQuotaIdentity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			v, excluded := currentFactsAccount(t, repo, account.ProviderWeb)
			write := func(tier account.WebTier, mode string, remaining int) {
				t.Helper()
				if err := saveQuotaWindowsFixture(repo, ctx, v.ID, tier, time.Now(), []account.QuotaWindow{{AccountID: v.ID, Mode: mode, Remaining: remaining, Total: 100, Source: account.QuotaSourceUpstream}}); err != nil {
					t.Fatal(err)
				}
			}
			write(account.WebTierBasic, account.QuotaModeWebImagePro, 40)
			selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
			session, err := selector.beginSelectionSession(ctx, v.Provider, 0, "image-model", account.QuotaModeWebImageEdit, "affinity", excluded, false)
			if err != nil {
				t.Fatal(err)
			}
			write(account.WebTierSuper, account.QuotaModeWebImageEdit, 80)
			current, err := repo.GetRoutingCandidate(ctx, v.ID, v.Provider, 0, "image-model", account.QuotaModeWebImageEdit)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := session.Acquire(ctx, excluded, false)
			if err != nil || lease == nil {
				t.Fatalf("claim: %v", err)
			}
			defer lease.Release()
			if lease.Credential.WebTier != account.WebTierSuper || lease.QuotaMode != account.QuotaModeWebImageEdit || current.QuotaWindow == nil || lease.QuotaSnapshotVersion != current.QuotaWindow.SnapshotVersion {
				t.Fatalf("stale selected metadata: tier=%s mode=%s version=%d current=%+v", lease.Credential.WebTier, lease.QuotaMode, lease.QuotaSnapshotVersion, current.QuotaWindow)
			}
		})
	}
}

func TestSelectorRevalidatesQuotaRecoveryLane(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			for _, change := range []string{"risk", "scope", "peer_claimed", "reset", "pinned_new_recovery"} {
				t.Run(change, func(t *testing.T) {
					ctx := context.Background()
					v, excluded := currentFactsAccount(t, repo, account.ProviderBuild)
					if change != "pinned_new_recovery" {
						seeded, err := repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: time.Now().Add(-2 * account.FreeQuotaRecoveryPause)})
						if err != nil || !seeded.Applied {
							t.Fatalf("seed due recovery: %v", err)
						}
						v.QuotaRecoveryRevision = seeded.Ref.Revision
					}
					port := &currentClaimRepository{AccountRepository: repo}
					limiter := memory.NewConcurrencyLimiter()
					selector := NewSelector(port, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
					scope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeFree}
					var session *selectionSession
					var err error
					if change != "pinned_new_recovery" {
						session, err = selector.beginSelectionSessionForKey(ctx, v.Provider, 0, "grok-test", "", "", excluded, true, scope)
						if err != nil || len(session.probeCandidates) != 1 {
							t.Fatalf("probe snapshot: %v", err)
						}
					}
					port.before = func(id uint64) error {
						switch change {
						case "risk":
							_, err = repo.UpdateAdministration(ctx, id, repository.AccountAdminPatch{Risk: &repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: "manual"}})
						case "scope":
							entitled := true
							_, err = repo.UpdateAdministration(ctx, id, repository.AccountAdminPatch{BuildSuperEntitled: &entitled})
						case "peer_claimed":
							_, err = repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryProbeClaimed})
						case "reset":
							err = repo.ResetQuotaState(ctx, v.Provider, []uint64{id})
						case "pinned_new_recovery":
							_, err = repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: time.Now().Add(-2 * account.FreeQuotaRecoveryPause)})
						}
						return err
					}
					var lease *accountLease
					if session != nil {
						lease, err = session.Acquire(ctx, excluded, true)
					} else {
						lease, err = selector.AcquirePinnedForKey(ctx, v.Provider, v.ID, 0, "grok-test", "", true, scope)
					}
					if change == "reset" || change == "pinned_new_recovery" {
						if err != nil || lease == nil {
							t.Fatalf("current recovery lane not usable: %v", err)
						}
						if lease.QuotaProbe != (change == "pinned_new_recovery") {
							t.Errorf("wrong lane: probe=%t", lease.QuotaProbe)
						}
						if lease.QuotaProbe && (lease.QuotaRecoveryRef == nil || lease.QuotaRecoveryRef.Revision != v.QuotaRecoveryRevision+2) {
							t.Errorf("lost current recovery claim: %+v", lease.QuotaRecoveryRef)
						}
					} else {
						want := SelectionNoAccounts
						if change == "peer_claimed" {
							want = SelectionQuotaExhausted
						}
						if lease != nil || !isSelectionUnavailable(err, want) {
							t.Errorf("stale recovery admitted: lease=%v err=%v", lease, err)
						}
						if port.materials != 0 {
							t.Error("refused probe hydrated secrets")
						}
					}
					if lease != nil {
						lease.Release()
					}
					assertSelectorCapacityReleased(t, limiter, []account.Credential{v})
					state, stateErr := repo.GetQuotaRecovery(ctx, v.ID)
					switch change {
					case "risk", "scope":
						if stateErr != nil || state.Status != account.QuotaRecoveryStatusExhausted {
							t.Errorf("refused probe changed recovery: %s %v", state.Status, stateErr)
						}
					case "peer_claimed", "pinned_new_recovery":
						if stateErr != nil || state.Status != account.QuotaRecoveryStatusProbing {
							t.Errorf("lost probe owner: %s %v", state.Status, stateErr)
						}
					case "reset":
						if !errors.Is(stateErr, repository.ErrNotFound) {
							t.Errorf("reset resurrected recovery: %v", stateErr)
						}
					}
				})
			}
		})
	}
}

func TestSelectorCurrentFactsFailureHasNoClaimOrMaterial(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			v, excluded := currentFactsAccount(t, repo, account.ProviderBuild)
			for _, path := range []string{"ordinary", "session", "pinned"} {
				for _, failure := range []string{"canceled", "storage", "deadline"} {
					t.Run(path+"/"+failure, func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						port := &currentClaimRepository{AccountRepository: repo}
						limiter := memory.NewConcurrencyLimiter()
						selector := NewSelector(port, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
						var session *selectionSession
						var err error
						if path == "session" {
							session, err = selector.beginSelectionSession(ctx, v.Provider, 0, "grok-test", "", "", excluded, false)
							if err != nil {
								t.Fatal(err)
							}
						}
						want := errors.New("current routing storage unavailable")
						if failure == "canceled" {
							want = context.Canceled
						}
						if failure == "deadline" {
							want = context.DeadlineExceeded
						}
						port.before = func(uint64) error {
							if failure == "canceled" {
								cancel()
							}
							return want
						}
						var lease *accountLease
						if session != nil {
							lease, err = session.Acquire(ctx, excluded, false)
						} else if path == "pinned" {
							lease, err = selector.AcquirePinnedForKey(ctx, v.Provider, v.ID, 0, "grok-test", "", true, clientkeydomain.AccountScope{})
						} else {
							if s, serr := selector.beginSelectionSession(ctx, v.Provider, 0, "grok-test", "", "", excluded, false); serr != nil {
								lease, err = nil, serr
							} else {
								lease, err = s.Acquire(ctx, excluded, false)
							}
						}
						if lease != nil {
							lease.Release()
							t.Error("failed fact load acquired")
						}
						if !errors.Is(err, want) {
							t.Errorf("fact load lost failure: %v", err)
						}
						if port.materials != 0 {
							t.Error("failed fact load hydrated secrets")
						}
						assertSelectorCapacityReleased(t, limiter, []account.Credential{v})
					})
				}
			}
		})
	}
}

func TestSelectorObservesCancellationAfterCurrentRead(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			v, excluded := currentFactsAccount(t, repo, account.ProviderBuild)
			for _, stage := range []string{"facts", "material"} {
				for _, path := range []string{"ordinary", "session", "pinned"} {
					t.Run(stage+"/"+path, func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						port := &currentClaimRepository{AccountRepository: repo}
						limiter := memory.NewConcurrencyLimiter()
						selector := NewSelector(port, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
						var session *selectionSession
						var err error
						if path == "session" {
							session, err = selector.beginSelectionSession(ctx, v.Provider, 0, "grok-test", "", "", excluded, false)
							if err != nil {
								t.Fatal(err)
							}
						}
						// Actual SQL succeeded. Cancellation is now visible to the
						// caller even though this repository operation returns nil.
						if stage == "facts" {
							port.afterFacts = cancel
						} else {
							port.afterMaterial = cancel
						}
						var lease *accountLease
						if session != nil {
							lease, err = session.Acquire(ctx, excluded, false)
						} else if path == "pinned" {
							lease, err = selector.AcquirePinnedForKey(ctx, v.Provider, v.ID, 0, "grok-test", "", true, clientkeydomain.AccountScope{})
						} else {
							if s, serr := selector.beginSelectionSession(ctx, v.Provider, 0, "grok-test", "", "", excluded, false); serr != nil {
								lease, err = nil, serr
							} else {
								lease, err = s.Acquire(ctx, excluded, false)
							}
						}
						if lease != nil {
							lease.Release()
							t.Error("observably canceled caller received lease")
						}
						if !errors.Is(err, context.Canceled) {
							t.Errorf("lost caller cancellation: %v", err)
						}
						wantMaterials := 0
						if stage == "material" {
							wantMaterials = 1
						}
						if port.materials != wantMaterials {
							t.Errorf("material reads=%d want=%d", port.materials, wantMaterials)
						}
						assertSelectorCapacityReleased(t, limiter, []account.Credential{v})
					})
				}
			}
		})
	}
}
