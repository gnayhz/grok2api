package egress

import (
	"context"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	physical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"io"
	"log/slog"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestSessionHintRespectsEveryPoolStrategy(t *testing.T) {
	for _, strategy := range []domain.PoolStrategy{domain.PoolStrategyAffinity, domain.PoolStrategySessionReuse, domain.PoolStrategyRandom, domain.PoolStrategySticky, domain.PoolStrategyRotation, domain.PoolStrategyLeastUsed} {
		for _, isolated := range []bool{false, true} {
			for _, fresh := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/isolated=%t/fresh=%t", strategy, isolated, fresh), func(t *testing.T) {
					m, repo := newPoolTestManager(t)
					m.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
					t.Cleanup(func() { _ = m.Close(context.Background()) })
					m.UpdateAccountIsolatedConnections(isolated)
					const poolID = uint64(50101)
					ResetPoolStats(poolID)
					repo.pool[poolID] = domain.Pool{ID: poolID, Enabled: true, Strategy: domain.PoolStrategyAffinity}
					nodes := sessionTestNodes(1, 2, 3)
					for i := range nodes {
						nodes[i].ProxyPool = fresh
					}
					// Seed a valid affinity pin at member 3, then update the explicit
					// strategy. The old hint must not overrule the new policy.
					repo.member[poolID] = nodes[2:]
					ctx := WithBuildSession(context.Background(), "shared-session")
					acquire := func(ctx context.Context, account string) *Lease {
						t.Helper()
						lease, outcome, err := m.AcquirePoolRouted(WithAccountIdentity(ctx, account), domain.ScopeBuild, account, poolID, false, "")
						if err != nil || lease == nil || outcome != PoolRouteMember {
							t.Fatalf("pool acquisition: %v %v", outcome, err)
						}
						return lease
					}
					seed := acquire(ctx, "A")
					seed.Release()
					repo.pool[poolID] = domain.Pool{ID: poolID, Enabled: true, Strategy: strategy, RotationCursorNodeID: 2}
					repo.member[poolID] = nodes
					m.InvalidatePoolCache()
					seen := map[uint64]bool{}
					for i := range 100 {
						lease := acquire(ctx, fmt.Sprintf("account-%d", i%2))
						seen[lease.NodeID] = true
						wantDecision := SessionReuseAccepted
						if fresh {
							wantDecision = SessionReuseFresh
						}
						if got := lease.ConnectionPolicy(); got != (ConnectionPolicy{AccountIsolated: isolated, Fresh: fresh, SessionReuse: wantDecision}) {
							t.Fatalf("policy = %+v", got)
						}
						lease.Release()
					}
					switch strategy {
					case domain.PoolStrategyAffinity:
						if len(seen) != 1 || !seen[3] {
							t.Fatalf("affinity lost valid session pin: %v", seen)
						}
					case domain.PoolStrategySessionReuse:
						if len(seen) != 1 {
							t.Fatalf("session reuse changed exit across account switches: %v", seen)
						}
					case domain.PoolStrategySticky:
						if len(seen) != 1 || !seen[1] {
							t.Fatalf("sticky overruled by session: %v", seen)
						}
					case domain.PoolStrategyRotation:
						if len(seen) != 1 || !seen[2] {
							t.Fatalf("rotation cursor overruled by session: %v", seen)
						}
					default:
						if len(seen) != 3 {
							t.Fatalf("%s overruled by session: %v", strategy, seen)
						}
					}
					// Exclusions and qualification are applied before any pin.
					m.SetExitEligibility(stubExitEligibility{ineligible: map[uint64]bool{2: true}})
					lease := acquire(physical.WithNodeExclusions(ctx, map[uint64]struct{}{3: {}}), "B")
					defer lease.Release()
					if lease.NodeID != 1 {
						t.Fatalf("hint bypassed exclusions/eligibility: %d", lease.NodeID)
					}
				})
			}
		}
	}
}

func TestBuildSessionHintCannotPinOtherScopes(t *testing.T) {
	for _, pool := range []bool{false, true} {
		t.Run(fmt.Sprintf("pool=%t", pool), func(t *testing.T) {
			m, repo := newPoolTestManager(t)
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			nodes := sessionTestNodes(1, 2, 3)
			repo.nodes = nodes
			repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyAffinity}
			repo.member[1] = nodes
			acquire := func(ctx context.Context, scope domain.Scope, account string) *Lease {
				t.Helper()
				var lease *Lease
				var err error
				if pool {
					lease, _, err = m.AcquirePoolRouted(ctx, scope, account, 1, false, "")
				} else {
					lease, err = m.Acquire(ctx, scope, account)
				}
				if err != nil || lease == nil {
					t.Fatalf("acquire: %v", err)
				}
				return lease
			}
			hint := WithBuildSession(context.Background(), "build-only-session")
			seed := acquire(hint, domain.ScopeBuild, "A")
			seed.Release()
			for _, scope := range []domain.Scope{domain.ScopeWeb, domain.ScopeWebAsset, domain.ScopeConsole, domain.ScopeConsoleAsset} {
				found := false
				for i := range 100 {
					account := fmt.Sprintf("account-%d", i)
					plain := acquire(context.Background(), scope, account)
					plain.Release()
					if plain.NodeID == seed.NodeID {
						continue
					}
					withHint := acquire(hint, scope, account)
					withHint.Release()
					if withHint.NodeID != plain.NodeID || withHint.client != plain.client {
						t.Fatalf("Build hint changed %s routing/client: %d -> %d", scope, plain.NodeID, withHint.NodeID)
					}
					if got := withHint.ConnectionPolicy().SessionReuse; got != SessionReuseUnsupported {
						t.Fatalf("unsupported scope decision=%q", got)
					}
					found = true
					break
				}
				if !found {
					t.Fatalf("could not find another node for %s", scope)
				}
			}
		})
	}
}

func TestSessionHintDoesNotOverrideFixedTargetOrClearAutoPin(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	nodes := sessionTestNodes(1, 2)
	for i := range nodes {
		nodes[i].EncryptedProxyURL = encryptedProxy(t, cipher, fmt.Sprintf("http://127.0.0.1:%d", 13001+i))
	}
	m := NewManagerWithLimits(egressRepositoryTestStub{nodes: nodes}, cipher, netbudget.Limits{})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	ctx := WithBuildSession(context.Background(), "same-history")
	first, err := m.Acquire(ctx, domain.ScopeBuild, "A")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	otherID := uint64(1)
	if first.NodeID == otherID {
		otherID = 2
	}
	fixed, err := m.Acquire(WithPinnedNode(ctx, otherID), domain.ScopeBuild, "B")
	if err != nil {
		t.Fatal(err)
	}
	defer fixed.Release()
	if fixed.NodeID != otherID || fixed.client == first.client {
		t.Fatal("session overruled fixed target or reused another node's transport")
	}
	auto, err := m.Acquire(ctx, domain.ScopeBuild, "B")
	if err != nil {
		t.Fatal(err)
	}
	defer auto.Release()
	if auto.NodeID != first.NodeID || auto.client != first.client {
		t.Fatal("temporary fixed target cleared independent automatic session pin")
	}
}
