package relational

import (
	"context"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type grantCostAdapter struct {
	face  account.Provider
	calls int
}

func (a *grantCostAdapter) Provider() account.Provider { return a.face }
func (a *grantCostAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: a.face, Credential: provider.CredentialSurface{AuthType: account.AuthTypeOAuth, DeviceOAuth: true}}
}
func (*grantCostAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, fmt.Errorf("not used")
}
func (a *grantCostAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return a.grant(), nil
}
func (a *grantCostAdapter) ConvertToBuild(context.Context, account.Credential) (provider.CredentialSeed, error) {
	return a.grant(), nil
}
func (a *grantCostAdapter) grant() provider.CredentialSeed {
	a.calls++
	return provider.CredentialSeed{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "grant-cost", SourceKey: "grant-cost", AccessToken: "cost-access", RefreshToken: "cost-refresh", ExpiresAt: time.Now().Add(time.Hour)}
}

// Same source runs on G23 and G24 through actual M07/SQL and optional Redis.
// Provider is an immediate fact; setup and device-session creation are untimed.
func BenchmarkAccountGrantCost(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"device_memory", "device_redis", "conversion_all"} {
			b.Run(dialect+"/"+mode, func(b *testing.B) {
				ctx := context.Background()
				db := webProfileCostDatabase(b, dialect)
				repo := NewAccountRepository(db)
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					b.Fatal(err)
				}
				var sessions repository.DeviceSessionRepository = memory.NewDeviceSessionStore()
				if mode == "device_redis" {
					address := os.Getenv("TEST_REDIS_ADDRESS")
					if address == "" {
						b.Skip("TEST_REDIS_ADDRESS required")
					}
					cfg := redisruntime.Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), KeyPrefix: fmt.Sprintf("g24-cost-%d:", time.Now().UnixNano())}
					if raw := os.Getenv("TEST_REDIS_DATABASE"); raw != "" {
						cfg.Database, err = strconv.Atoi(raw)
						if err != nil {
							b.Fatal(err)
						}
					}
					runtime, err := redisruntime.Open(ctx, cfg)
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() { _ = runtime.Close() })
					sessions = redisruntime.NewDeviceSessionStore(runtime)
				}
				adapter := &grantCostAdapter{face: account.ProviderBuild}
				var web account.Credential
				if mode == "conversion_all" {
					adapter.face = account.ProviderWeb
					web, _, err = repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web-cost", SourceKey: "web-cost", EncryptedAccessToken: "synthetic", AuthStatus: account.AuthStatusActive})
					if err != nil {
						b.Fatal(err)
					}
				}
				service := accountapp.NewService(repo, NewAuditRepository(db), sessions, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, memory.NewLockStore())
				service.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
				operations := 0
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if mode == "conversion_all" {
						result, err := service.ConvertWebAccountsToBuildWithStrategy(ctx, []uint64{web.ID}, accountapp.BuildConversionAll, nil, nil)
						if err != nil || result.Failed != 0 || result.Skipped != 0 || len(result.BuildAccountIDs) != 1 {
							b.Fatalf("conversion %+v %v", result, err)
						}
					} else {
						b.StopTimer()
						if err := sessions.Create(ctx, account.DeviceSession{ID: "cost", DeviceCode: "cost-device", Interval: time.Second, NextPollAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
							b.Fatal(err)
						}
						b.StartTimer()
						if _, err := service.PollDeviceLogin(ctx, "cost"); err != nil {
							b.Fatal(err)
						}
					}
					operations++
				}
				b.StopTimer()
				if adapter.calls != operations {
					b.Fatalf("provider calls=%d operations=%d", adapter.calls, operations)
				}
			})
		}
	}
}
