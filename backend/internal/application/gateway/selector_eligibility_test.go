package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Only this optional port is synthetic. Candidates, current materials and
// capacity all run their actual SQL/runtime implementations.
type mutableSelectorEligibility struct{ denied atomic.Uint64 }

func (e *mutableSelectorEligibility) AccountSchedulable(id uint64) bool {
	denied := e.denied.Load()
	return denied != ^uint64(0) && denied != id
}

type selectorAdmissionAuthority struct {
	*mutableSelectorEligibility
	check func(context.Context, uint64) (bool, error)
}

func (e selectorAdmissionAuthority) CheckAccountAdmission(ctx context.Context, id uint64) (bool, error) {
	return e.check(ctx, id)
}

type eligibilityMaterialRepository struct {
	*relational.AccountRepository
	after func(uint64)
}

func (r *eligibilityMaterialRepository) GetCredentialMaterial(ctx context.Context, id uint64, provider account.Provider) (account.CredentialMaterial, error) {
	value, err := r.AccountRepository.GetCredentialMaterial(ctx, id, provider)
	if err == nil && r.after != nil {
		r.after(id)
	}
	return value, err
}

func selectorEligibilityAccounts(t *testing.T, repo *relational.AccountRepository, count int) ([]account.Credential, map[uint64]bool) {
	t.Helper()
	ctx := context.Background()
	excluded := make(map[uint64]bool)
	existing, err := repo.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-test", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range existing {
		excluded[v.Credential.ID] = true
	}
	values := make([]account.Credential, count)
	for i := range values {
		name := fmt.Sprintf("%s-%d-%d", t.Name(), time.Now().UnixNano(), i)
		v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "synthetic", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100 - i, MaxConcurrent: 4})
		if err != nil {
			t.Fatal(err)
		}
		values[i] = v
	}
	return values, excluded
}

func assertSelectorCapacityReleased(t *testing.T, limiter repository.ConcurrencyLimiter, values []account.Credential) {
	t.Helper()
	for _, v := range values {
		current, err := limiter.Current(context.Background(), repository.AccountConcurrencyKey(v.ID))
		if err != nil || current != 0 {
			t.Fatalf("account %d capacity=%d err=%v", v.ID, current, err)
		}
	}
}

func TestSelectorOptionalEligibilityAtEveryClaim(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			values, excluded := selectorEligibilityAccounts(t, repo, 100)
			for _, path := range []string{"ordinary", "pinned", "session", "retry", "segmented", "session_segmented"} {
				stages := []string{"before_filter", "during_material", "skip_first"}
				if path == "session" || path == "retry" || path == "session_segmented" {
					stages = append(stages, "after_snapshot")
				}
				for _, stage := range stages {
					t.Run(path+"/"+stage, func(t *testing.T) {
						ctx := context.Background()
						eligibility := &mutableSelectorEligibility{}
						material := &eligibilityMaterialRepository{AccountRepository: repo}
						limiter := memory.NewConcurrencyLimiter()
						selector := NewSelector(material, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
						selector.SetQualityEligibility(eligibility)
						selector.UpdateConfig(time.Hour, time.Second, time.Minute, 0)
						if strings.Contains(path, "segmented") {
							selector.UpdateSegmentedSelector(true, 100, 8)
						}
						if stage == "before_filter" {
							eligibility.denied.Store(^uint64(0))
						}
						var session *selectionSession
						var err error
						if path == "session" || path == "retry" || path == "session_segmented" {
							session, err = selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false)
							if stage == "before_filter" {
								var unavailable *SelectionUnavailableError
								if !errors.As(err, &unavailable) || unavailable.Reason != SelectionNoAccounts {
									t.Fatalf("initial refusal=%v", err)
								}
								assertSelectorCapacityReleased(t, limiter, values)
								return
							}
							if err != nil {
								t.Fatal(err)
							}
							if path == "retry" {
								lease, err := session.Acquire(ctx, excluded, false)
								if err != nil || lease == nil {
									t.Fatalf("initial retry lease: %v", err)
								}
								id := lease.Credential.ID
								lease.Release()
								session.RetryAccount(id)
							}
						}
						if stage == "after_snapshot" {
							eligibility.denied.Store(^uint64(0))
						}
						if stage == "during_material" || stage == "skip_first" {
							material.after = func(id uint64) {
								if stage == "during_material" {
									eligibility.denied.Store(^uint64(0))
								} else {
									eligibility.denied.Store(values[0].ID)
								}
							}
						}
						var lease *accountLease
						if session != nil {
							lease, err = session.Acquire(ctx, excluded, false)
						} else if path == "pinned" {
							lease, err = selector.AcquirePinned(ctx, account.ProviderBuild, values[0].ID, 0, "grok-test", "", false)
						} else {
							lease, err = selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false)
						}
						if stage == "skip_first" && path != "pinned" {
							if err != nil || lease == nil {
								t.Fatalf("remaining eligible candidate lost: %v", err)
							}
							if lease.Credential.ID != values[1].ID {
								t.Errorf("acquired %d, want remaining account %d", lease.Credential.ID, values[1].ID)
							}
							if strings.Contains(path, "segmented") && lease.selectorObservation == nil {
								t.Error("segmented path was not exercised")
							}
						} else {
							if err == nil || lease != nil {
								t.Errorf("denied account escaped %s/%s: lease=%v error=%v", path, stage, lease, err)
							}
							if stage == "before_filter" {
								var unavailable *SelectionUnavailableError
								if !errors.As(err, &unavailable) || unavailable.Reason != SelectionNoAccounts {
									t.Errorf("initial refusal=%v", err)
								}
							}
						}
						if lease != nil {
							lease.Release()
						}
						assertSelectorCapacityReleased(t, limiter, values)
					})
				}
			}
		})
	}
}

func TestSelectorAuthoritativeAdmissionSupersedesFinalHint(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			values, excluded := selectorEligibilityAccounts(t, repo, 1)
			for _, path := range []string{"ordinary", "session", "pinned", "quality_probe"} {
				for _, outcome := range []string{"allow_stale_hint", "deny", "error", "canceled", "nil"} {
					t.Run(path+"/"+outcome, func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						limiter := memory.NewConcurrencyLimiter()
						eligibility := &mutableSelectorEligibility{}
						material := &eligibilityMaterialRepository{AccountRepository: repo, after: func(uint64) { eligibility.denied.Store(^uint64(0)) }}
						selector := NewSelector(material, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
						selector.UpdateConfig(time.Hour, time.Second, time.Minute, 0)
						storageErr := errors.New("durable eligibility unavailable")
						var checks int
						if outcome != "nil" {
							selector.SetQualityEligibility(selectorAdmissionAuthority{mutableSelectorEligibility: eligibility, check: func(ctx context.Context, id uint64) (bool, error) {
								checks++
								if current, err := limiter.Current(ctx, repository.AccountConcurrencyKey(id)); err != nil || current != 1 {
									t.Fatalf("admission did not own capacity: %d %v", current, err)
								}
								switch outcome {
								case "deny":
									return false, nil
								case "error":
									return false, storageErr
								case "canceled":
									cancel()
									return false, ctx.Err()
								}
								return true, nil
							}})
						}
						var lease *accountLease
						var err error
						switch path {
						case "session":
							var session *selectionSession
							session, err = selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false)
							if err == nil {
								lease, err = session.Acquire(ctx, excluded, false)
							}
						case "pinned":
							lease, err = selector.AcquirePinned(ctx, account.ProviderBuild, values[0].ID, 0, "grok-test", "", false)
						case "quality_probe":
							lease, err = selector.AcquirePinnedForQualityProbe(ctx, account.ProviderBuild, values[0].ID, 0, "grok-test", "", clientkeydomain.AccountScope{})
						default:
							lease, err = selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false)
						}
						allowed := path == "quality_probe" || outcome == "allow_stale_hint" || outcome == "nil"
						if allowed {
							if err != nil || lease == nil {
								t.Errorf("valid claim refused: %v", err)
							}
						} else {
							if err == nil || lease != nil {
								t.Errorf("restriction escaped: %v %v", lease, err)
							}
							if outcome == "error" && !errors.Is(err, storageErr) {
								t.Errorf("lost durable error: %v", err)
							}
							if outcome == "canceled" && !errors.Is(err, context.Canceled) {
								t.Errorf("lost cancellation: %v", err)
							}
						}
						wantChecks := 1
						if path == "quality_probe" || outcome == "nil" {
							wantChecks = 0
						}
						if checks != wantChecks {
							t.Errorf("authoritative checks=%d want=%d", checks, wantChecks)
						}
						if lease != nil {
							lease.Release()
						}
						assertSelectorCapacityReleased(t, limiter, values)
					})
				}
			}
		})
	}
}

func TestSelectionSessionQuotaRecoveryRechecksOptionalEligibility(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			values, excluded := selectorEligibilityAccounts(t, repo, 1)
			v := values[0]
			seed, err := repo.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: time.Now().Add(-25 * time.Hour), Used: 100, Limit: 100})
			if err != nil || !seed.Applied {
				t.Fatalf("seed: %v", err)
			}
			limiter := memory.NewConcurrencyLimiter()
			selector := NewSelector(repo, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
			selector.UpdateConfig(time.Hour, time.Second, time.Minute, 0)
			eligibility := &mutableSelectorEligibility{}
			selector.SetQualityEligibility(eligibility)
			session, err := selector.beginSelectionSession(ctx, v.Provider, 0, "grok-test", "", "", excluded, true)
			if err != nil {
				t.Fatal(err)
			}
			eligibility.denied.Store(v.ID)
			lease, err := session.Acquire(ctx, excluded, true)
			if lease != nil {
				lease.Release()
				t.Fatal("quality restriction escaped through quota recovery")
			}
			if err == nil {
				t.Fatal("expected refusal")
			}
			recovery, err := repo.GetQuotaRecovery(ctx, v.ID)
			if err != nil || recovery.Status != account.QuotaRecoveryStatusExhausted {
				t.Fatalf("denied request consumed recovery claim: %+v %v", recovery, err)
			}
			assertSelectorCapacityReleased(t, limiter, values)
		})
	}
}
