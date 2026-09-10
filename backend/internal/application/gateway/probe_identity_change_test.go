package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Real link mutations and two registry snapshots must not create new capacity
// for an already running Build measurement. Production MaxConcurrent may be >1.
func TestProbeIdentityChangeKeepsAccountCapacity(t *testing.T) {
	for _, runtime := range []string{"memory", "redis"} {
		t.Run(runtime, func(t *testing.T) {
			ctx := context.Background()
			var firstLimiter, secondLimiter repository.ConcurrencyLimiter
			if runtime == "redis" {
				address := os.Getenv("TEST_REDIS_ADDRESS")
				if address == "" {
					t.Skip("TEST_REDIS_ADDRESS requires isolated Redis")
				}
				cfg := redisruntime.Config{Address: address, KeyPrefix: "e10:" + time.Now().Format("150405.000000000") + ":"}
				first, err := redisruntime.Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = first.Close() })
				second, err := redisruntime.Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = second.Close() })
				firstLimiter, secondLimiter = redisruntime.NewConcurrencyLimiter(first), redisruntime.NewConcurrencyLimiter(second)
			} else {
				firstLimiter = memory.NewConcurrencyLimiter()
				secondLimiter = firstLimiter
			}
			for _, change := range []string{"link", "delete_peer", "peer_snapshot_stale"} {
				t.Run(change, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "identity.db")
					db, err := relational.OpenSQLite(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = db.Close() })
					if err := db.InitializeSchema(ctx); err != nil {
						t.Fatal(err)
					}
					accounts := relational.NewAccountRepository(db)
					refs := make(map[uint64]account.CredentialRef)
					add := func(provider account.Provider, key string) uint64 {
						c, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: provider, Name: key, SourceKey: key, EncryptedAccessToken: "test", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 4})
						if err != nil {
							t.Fatal(err)
						}
						refs[c.ID] = c.CredentialRef()
						return c.ID
					}
					build, web := add(account.ProviderBuild, "build"), add(account.ProviderWeb, "web")
					if change == "delete_peer" {
						if err := accounts.LinkWebToBuild(ctx, refs[web], refs[build]); err != nil {
							t.Fatal(err)
						}
					}
					open := func() *registry.Registry {
						r, err := registry.Open(ctx, registry.Options{SQLitePath: path, AccountLinks: accounts})
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = r.Close() })
						return r
					}
					a, b := open(), open()
					identityContext := func(r *registry.Registry, parent context.Context) context.Context {
						group, members := r.IdentityGroupOf(build)
						return model.WithProbeIdentity(parent, group, len(members) > 1)
					}
					first, second := &Selector{concurrency: firstLimiter}, &Selector{concurrency: secondLimiter}
					release, err := first.acquireProbeResources(identityContext(a, ctx), build)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					if change == "delete_peer" {
						if err := accounts.Delete(ctx, web); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := accounts.LinkWebToBuild(ctx, refs[web], refs[build]); err != nil {
							t.Fatal(err)
						}
					}
					if err := b.RefreshIdentityGroups(ctx); err != nil {
						t.Fatal(err)
					}
					// Hold the refreshed group, then use A's stale unlinked snapshot.
					if change == "peer_snapshot_stale" {
						release()
						release, err = first.acquireProbeResources(identityContext(b, ctx), build)
						if err != nil {
							t.Fatal(err)
						}
						defer release()
						b = a
					}
					deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
					extra, err := second.acquireProbeResources(identityContext(b, deadline), build)
					if extra != nil {
						extra()
					}
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("same Build account overlapped after %s: err=%v", change, err)
					}
					release()
					next, err := second.acquireProbeResources(identityContext(b, ctx), build)
					if err != nil {
						t.Fatal(err)
					}
					next()
				})
			}
		})
	}
}
