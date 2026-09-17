package selector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

func TestRedisHealthProjectionPreservesSQLRevision(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	databaseNumber := 0
	if value := os.Getenv("TEST_REDIS_DATABASE"); value != "" {
		var err error
		databaseNumber, err = strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := redisruntime.Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: databaseNumber, KeyPrefix: fmt.Sprintf("e01-health:%d:", time.Now().UnixNano()), ConcurrencyLease: time.Minute}
	cleanup := redisclient.NewClient(&redisclient.Options{Addr: address, Username: cfg.Username, Password: cfg.Password, DB: databaseNumber})
	defer cleanup.Close()
	defer func() {
		cleanCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		it := cleanup.Scan(cleanCtx, 0, cfg.KeyPrefix+"*", 100).Iterator()
		for it.Next(cleanCtx) {
			if err := cleanup.Del(cleanCtx, it.Val()).Err(); err != nil {
				t.Error(err)
			}
		}
		if err := it.Err(); err != nil {
			t.Error(err)
		}
	}()
	a, err := redisruntime.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := redisruntime.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "health", SourceKey: "health", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	values, err := selector.loadCandidates(ctx, v.Provider, 0, "grok", "", time.Now().UTC())
	if err != nil || len(values) != 1 {
		t.Fatalf("load: %v %v", values, err)
	}
	updates := make(chan repository.InvalidationEvent, 16)
	done := make(chan error, 1)
	go func() {
		done <- b.ListenInvalidations(ctx, func(_ context.Context, e repository.InvalidationEvent) error {
			selector.ApplyInvalidation(e)
			select {
			case updates <- e:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	defer func() { cancel(); <-done }()
	ready := repository.InvalidationEvent{Kind: repository.InvalidationAccountHealthChanged, AccountID: v.ID, Provider: v.Provider}
	for {
		if err := a.PublishInvalidation(ctx, ready); err != nil {
			t.Fatal(err)
		}
		select {
		case <-updates:
			goto listening
		case <-ctx.Done():
			t.Fatal("subscription unavailable")
		case <-time.After(15 * time.Millisecond):
		}
	}
listening:
	var committed []repository.InvalidationEvent
	repo.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { committed = append(committed, e) })
	for range 2 {
		if _, err := repo.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthFailure, Status: 429, CooldownBase: time.Second, CooldownMax: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	if len(committed) != 2 || committed[1].HealthRevision != 2 {
		t.Fatalf("missing committed state: %+v", committed)
	}
	var transport uint64
	for _, index := range []int{1, 0} {
		if err := a.PublishInvalidation(ctx, committed[index]); err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case received := <-updates:
				if received.HealthRevision == committed[index].HealthRevision {
					if received.Revision <= transport {
						t.Fatal("transport revision did not advance")
					}
					transport = received.Revision
					goto received
				}
			case <-ctx.Done():
				t.Fatal("state notification unavailable")
			}
		}
	received:
		got := selector.applyRoutingHealth(values[0].Credential, time.Now().UTC())
		if got.HealthRevision != 2 || got.FailureCount != 2 || got.CooldownUntil == nil {
			t.Fatalf("remote health regressed: revision=%d failures=%d", got.HealthRevision, got.FailureCount)
		}
	}
	if values[0].Credential.HealthRevision != 0 || values[0].Credential.FailureCount != 0 {
		t.Fatal("immutable candidate mutated")
	}
}
