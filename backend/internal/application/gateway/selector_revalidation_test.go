package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestSelectionSessionRejectsCommittedHardRestriction(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			for _, scenario := range []string{"risk", "tier_scope"} {
				t.Run(scenario, func(t *testing.T) {
					ctx := context.Background()
					db := openRoutingCancellationDB(t, dialect)
					repo := relational.NewAccountRepository(db)
					v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierBasic, Name: scenario, SourceKey: scenario, EncryptedAccessToken: "synthetic", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
					if err != nil {
						t.Fatal(err)
					}
					limiter := memory.NewConcurrencyLimiter()
					selector := NewSelector(repo, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
					repo.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { selector.ApplyInvalidation(e) })
					scope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeWeb, Tiers: clientkeydomain.TierScopeFree}
					session, err := selector.beginSelectionSessionForKey(ctx, v.Provider, 0, "grok-test", "", "", nil, false, scope)
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "risk" {
						_, err = repo.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{Risk: &repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: "manual"}})
					} else {
						err = saveQuotaWindowsFixture(repo, ctx, v.ID, account.WebTierSuper, time.Now(), nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					current, err := repo.Get(ctx, v.ID)
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "risk" && current.RiskStatus == "" || scenario == "tier_scope" && current.WebTier != account.WebTierSuper {
						t.Fatal("restriction not committed")
					}
					// A new ordinary request sees the committed restriction, but this session
					// must also apply it before selecting a new physical attempt.
					normal, err := selector.AcquireForKey(ctx, v.Provider, 0, "grok-test", "", "", nil, false, scope)
					if normal != nil {
						normal.Release()
						t.Fatal("new request unexpectedly eligible")
					}
					if err == nil {
						t.Fatal("new request missing refusal")
					}
					lease, err := session.Acquire(ctx, nil, false)
					if lease != nil {
						lease.Release()
						t.Fatalf("session leased account after committed %s; SQL risk=%q tier=%q", scenario, current.RiskStatus, current.WebTier)
					}
					if err == nil {
						t.Fatal("expected session refusal")
					}
				})
			}
		})
	}
}

// The hook commits through M07 immediately before the actual selected-account
// read. Initial candidate filtering/planning has already happened on every path.
type currentClaimRepository struct {
	*relational.AccountRepository
	afterFacts       func()
	afterMaterial    func()
	before           func(uint64) error
	reads, materials int
	materialIDs      []uint64
}

func (r *currentClaimRepository) GetRoutingCandidate(ctx context.Context, id uint64, provider account.Provider, route uint64, model, mode string) (account.RoutingCandidate, error) {
	r.reads++
	if r.before != nil {
		before := r.before
		r.before = nil
		if err := before(id); err != nil {
			return account.RoutingCandidate{}, err
		}
	}
	value, err := r.AccountRepository.GetRoutingCandidate(ctx, id, provider, route, model, mode)
	if err == nil && r.afterFacts != nil {
		r.afterFacts()
	}
	return value, err
}

func (r *currentClaimRepository) GetCredentialMaterial(ctx context.Context, id uint64, provider account.Provider) (account.CredentialMaterial, error) {
	r.materials++
	r.materialIDs = append(r.materialIDs, id)
	value, err := r.AccountRepository.GetCredentialMaterial(ctx, id, provider)
	if err == nil && r.afterMaterial != nil {
		r.afterMaterial()
	}
	return value, err
}

func TestSelectorRevalidatesScopeAndRiskOnEveryClaimPath(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			values := make([]account.Credential, 100)
			for i := range values {
				name := fmt.Sprintf("g19-%s-%d-%d", dialect, time.Now().UnixNano(), i)
				v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
					Name: name, SourceKey: name, WebTier: account.WebTierBasic, Enabled: true, AuthStatus: account.AuthStatusActive,
					EncryptedAccessToken: "synthetic", Priority: 1000 - i, MaxConcurrent: 4})
				if err != nil {
					t.Fatal(err)
				}
				values[i] = v
			}
			scope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeWeb, Tiers: clientkeydomain.TierScopeFree}
			for _, path := range []string{"ordinary", "pinned", "pinned_read", "quality_probe", "sticky", "session", "retry", "session_sticky", "segmented", "session_segmented"} {
				for _, notify := range []bool{false, true} {
					for _, restriction := range []string{"risk", "tier_scope"} {
						t.Run(fmt.Sprintf("%s/notify=%t/%s", path, notify, restriction), func(t *testing.T) {
							port := &currentClaimRepository{AccountRepository: repo}
							limiter := memory.NewConcurrencyLimiter()
							sticky := memory.NewStickyStore()
							selector := NewSelector(port, limiter, sticky, nil, time.Hour, time.Second, time.Minute)
							selector.UpdateConfig(time.Hour, time.Second, time.Minute, 0)
							if strings.Contains(path, "segmented") {
								selector.UpdateSegmentedSelector(true, 100, 8)
							}
							repo.SetInvalidationObserver(nil)
							if notify {
								repo.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { selector.ApplyInvalidation(e) })
							}
							defer repo.SetInvalidationObserver(nil)
							affinity := ""
							if strings.Contains(path, "sticky") {
								affinity = "g19-session"
								if err := sticky.Set(ctx, stickySessionKey(affinity), values[0].ID, time.Now().Add(time.Hour)); err != nil {
									t.Fatal(err)
								}
							}
							var session *selectionSession
							var err error
							if strings.HasPrefix(path, "session") || path == "retry" {
								session, err = selector.beginSelectionSessionForKey(ctx, account.ProviderWeb, 0, "grok-test", "", affinity, nil, false, scope)
								if err != nil {
									t.Fatal(err)
								}
								if path == "retry" {
									lease, err := session.Acquire(ctx, nil, false)
									if err != nil || lease == nil {
										t.Fatalf("first lease: %v", err)
									}
									if lease.Credential.ID != values[0].ID {
										t.Fatal("unexpected retry origin")
									}
									lease.Release()
									session.RetryAccount(values[0].ID)
									port.materials, port.materialIDs = 0, nil
								}
							}
							committed := false
							port.before = func(id uint64) error {
								if id != values[0].ID {
									return fmt.Errorf("claim started at %d, want %d", id, values[0].ID)
								}
								if restriction == "risk" {
									_, err = repo.UpdateAdministration(ctx, id, repository.AccountAdminPatch{Risk: &repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: "manual"}})
								} else {
									err = saveQuotaWindowsFixture(repo, ctx, id, account.WebTierSuper, time.Now(), nil)
								}
								committed = err == nil
								return err
							}
							defer func() {
								if _, err := repo.UpdateAdministration(ctx, values[0].ID, repository.AccountAdminPatch{Risk: &repository.RiskAttribution{}}); err != nil {
									t.Error(err)
								}
								if err := saveQuotaWindowsFixture(repo, ctx, values[0].ID, account.WebTierBasic, time.Now(), nil); err != nil {
									t.Error(err)
								}
							}()
							var lease *accountLease
							switch {
							case session != nil:
								lease, err = session.Acquire(ctx, nil, false)
							case path == "quality_probe":
								lease, err = selector.AcquirePinnedForQualityProbe(ctx, account.ProviderWeb, values[0].ID, 0, "grok-test", "", scope)
							case strings.HasPrefix(path, "pinned"):
								lease, err = selector.AcquirePinnedForKey(ctx, account.ProviderWeb, values[0].ID, 0, "grok-test", "", path != "pinned_read", scope)
							default:
								lease, err = selector.AcquireForKey(ctx, account.ProviderWeb, 0, "grok-test", "", affinity, nil, false, scope)
							}
							if !committed {
								t.Error("claim never reached the committed mutation")
							}
							fixed := strings.HasPrefix(path, "pinned") || path == "quality_probe"
							if fixed {
								var unavailable *SelectionUnavailableError
								if lease != nil || !errors.As(err, &unavailable) || unavailable.Reason != SelectionNoAccounts || unavailable.Scope != scope {
									t.Errorf("fixed refusal: lease=%v err=%v", lease, err)
								}
								if port.materials != 0 {
									t.Errorf("denied account loaded %d materials", port.materials)
								}
							} else {
								if err != nil || lease == nil {
									t.Fatalf("legal fallback lost: %v", err)
								}
								if lease.Credential.ID != values[1].ID {
									t.Errorf("leased %d, want fallback %d", lease.Credential.ID, values[1].ID)
								}
								if strings.Contains(path, "segmented") && lease.selectorObservation == nil {
									t.Error("segmented path not exercised")
								}
								if port.materials != 1 || port.materialIDs[0] != values[1].ID {
									t.Errorf("material hydration=%v", port.materialIDs)
								}
							}
							if lease != nil {
								lease.Release()
							}
							assertSelectorCapacityReleased(t, limiter, values)
						})
					}
				}
			}
		})
	}
}

func TestSelectorStickyRebindingRechecksCurrentTarget(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			for _, path := range []string{"ordinary", "session"} {
				t.Run(path, func(t *testing.T) {
					ctx := context.Background()
					first, excluded := currentFactsAccount(t, repo, account.ProviderBuild)
					second, _ := currentFactsAccount(t, repo, account.ProviderBuild)
					priority := 100
					if _, err := repo.UpdateAdministration(ctx, first.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Priority: &priority}}); err != nil {
						t.Fatal(err)
					}
					sticky := memory.NewStickyStore()
					limiter := memory.NewConcurrencyLimiter()
					port := &eligibilityMaterialRepository{AccountRepository: repo}
					selector := NewSelector(port, limiter, sticky, nil, time.Hour, time.Second, time.Minute)
					const affinity = "rebind-current-target"
					switched := false
					port.after = func(id uint64) {
						if switched {
							return
						}
						switched = true
						if id != first.ID {
							t.Error("wrong initial claim")
						}
						// A peer binds another account while this request finishes its
						// first claim; that target is restricted before Bind returns it.
						if _, err := repo.UpdateAdministration(ctx, second.ID, repository.AccountAdminPatch{Risk: &repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: "manual"}}); err != nil {
							t.Error(err)
						}
						if err := sticky.Set(ctx, stickySessionKey(affinity), second.ID, time.Now().Add(time.Hour)); err != nil {
							t.Error(err)
						}
					}
					var lease *accountLease
					var err error
					if path == "session" {
						session, err := selector.beginSelectionSession(ctx, first.Provider, 0, "grok-test", "", affinity, excluded, false)
						if err != nil {
							t.Fatal(err)
						}
						lease, err = session.Acquire(ctx, excluded, false)
					} else {
						lease, err = selector.Acquire(ctx, first.Provider, 0, "grok-test", "", affinity, excluded, false)
					}
					if err != nil || lease == nil {
						t.Fatalf("legal first lease lost: %v", err)
					}
					if lease.Credential.ID != first.ID {
						t.Error("rebound to a restricted account")
					}
					lease.Release()
					bound, ok, err := sticky.Get(ctx, stickySessionKey(affinity), time.Now())
					if err != nil || !ok || bound != first.ID {
						t.Errorf("invalid binding survived: %d %t %v", bound, ok, err)
					}
					assertSelectorCapacityReleased(t, limiter, []account.Credential{first, second})
				})
			}
		})
	}
}

func TestSelectorSegmentedHardRefusalDoesNotWaitForCapacity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := openRoutingCancellationDB(t, dialect)
			repo := relational.NewAccountRepository(db)
			values, excluded := selectorEligibilityAccounts(t, repo, 100)
			ids := make([]uint64, len(values))
			for i, v := range values {
				ids[i] = v.ID
			}
			for _, window := range []int{8, 100} {
				for _, sessionPath := range []bool{false, true} {
					t.Run(fmt.Sprintf("window=%d/session=%t", window, sessionPath), func(t *testing.T) {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						port := &currentClaimRepository{AccountRepository: repo}
						limiter := memory.NewConcurrencyLimiter()
						selector := NewSelector(port, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute, time.Minute)
						selector.UpdateSegmentedSelector(true, 100, window)
						var session *selectionSession
						var err error
						if sessionPath {
							session, err = selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false)
							if err != nil {
								t.Fatal(err)
							}
						}
						port.before = func(uint64) error {
							enabled := false
							_, err := repo.UpdateMany(ctx, account.ProviderBuild, ids, repository.AccountUpdates{Enabled: &enabled})
							return err
						}
						defer func() {
							enabled := true
							if _, err := repo.UpdateMany(context.Background(), account.ProviderBuild, ids, repository.AccountUpdates{Enabled: &enabled}); err != nil {
								t.Error(err)
							}
						}()
						var lease *accountLease
						if session != nil {
							lease, err = session.Acquire(ctx, excluded, false)
						} else {
							lease, err = selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false)
						}
						if lease != nil {
							lease.Release()
							t.Error("disabled cohort admitted")
						}
						if !isSelectionUnavailable(err, SelectionNoAccounts) {
							t.Errorf("hard refusal waited for capacity: %v", err)
						}
						if port.materials != 0 {
							t.Error("disabled cohort hydrated")
						}
						assertSelectorCapacityReleased(t, limiter, values)
					})
				}
			}
		})
	}
}
