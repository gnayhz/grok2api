package selector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

func TestQuotaProjectionOrderingAndImmutableRouting(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases[0].QuotaWindow = &account.QuotaWindow{AccountID: 1, Mode: "fast", Remaining: 10, SnapshotVersion: 5, Revision: 5}
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	values, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "fast", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	original := values[0].QuotaWindow
	event := func(mode string, snapshot, revision uint64, remaining int) repository.InvalidationEvent {
		return repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: 1, Quota: &account.QuotaProjection{Mode: mode, SnapshotVersion: snapshot, Revision: revision, Remaining: remaining}}
	}
	selector.ApplyInvalidation(event("fast", 5, 7, 2))
	selector.ApplyInvalidation(event("fast", 5, 6, 7))
	selector.ApplyInvalidation(event("fast", 5, 7, 9))
	selector.ApplyInvalidation(event("other", 5, 99, 0))
	if got := selector.applyQuotaProjection(values[0]); got.QuotaWindow.Remaining != 2 {
		t.Fatalf("out-of-order changed newest projection: %+v", got.QuotaWindow)
	}
	if original.Remaining != 10 {
		t.Fatal("published candidate mutated")
	}
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "fast", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	baseCalls, overlayCalls := repo.callCounts("model-a")
	if baseCalls != 1 || overlayCalls != 1 {
		t.Fatalf("numeric projection reloaded large pool: base=%d overlay=%d", baseCalls, overlayCalls)
	}
	selector.ApplyInvalidation(event("fast", 5, 8, 0))
	if !selector.quotaWindowExhausted(values[0]) {
		t.Fatal("known zero remained available")
	}
	// A subsequently loaded snapshot wins over an old absolute notification.
	newer := values[0]
	newer.QuotaWindow = &account.QuotaWindow{AccountID: 1, Mode: "fast", Remaining: 12, SnapshotVersion: 9, Revision: 9}
	if got := selector.applyQuotaProjection(newer); got.QuotaWindow.Remaining != 12 || selector.quotaWindowExhausted(newer) {
		t.Fatal("new snapshot inherited old reduction")
	}
	selector.ApplyInvalidation(event("fast", 9, 10, 11))
	lease := &accountLease{}
	selector.setLeaseQuota(lease, values[0], "fast")
	if lease.QuotaSnapshotVersion != 9 {
		t.Fatalf("selected outdated quota identity: %d", lease.QuotaSnapshotVersion)
	}
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			selector.ApplyInvalidation(event("fast", 9, uint64(11+i), 100-i))
			_ = selector.applyQuotaProjection(values[0])
		}()
	}
	wg.Wait()
	if got := selector.applyQuotaProjection(values[0]); got.QuotaWindow.Revision != 74 || got.QuotaWindow.Remaining != 37 {
		t.Fatalf("concurrent newest projection=%+v", got.QuotaWindow)
	}
	old := event("fast", 9, 75, 0)
	old.PublishedAt = time.Now().Add(-time.Hour)
	selector.ApplyInvalidation(old)
	if selector.applyQuotaProjection(values[0]).QuotaWindow.Revision != 74 {
		t.Fatal("expired notification overwrote current evidence")
	}
	selector.quotaProjectionMu.Lock()
	entry := selector.quotaProjections[quotaProjectionKey{1, "fast"}]
	entry.expiresAt = time.Now().Add(-time.Second)
	selector.quotaProjections[quotaProjectionKey{1, "fast"}] = entry
	selector.quotaProjectionMu.Unlock()
	if got := selector.applyQuotaProjection(newer); got.QuotaWindow.Revision != 9 {
		t.Fatal("expired projection survived")
	}
	for id := range uint64(maxQuotaProjections) {
		selector.quotaProjections[quotaProjectionKey{id + 2, "fast"}] = quotaProjectionEntry{value: account.QuotaProjection{Mode: "fast", SnapshotVersion: 1, Revision: 1, Remaining: 1}}
	}
	selector.ApplyInvalidation(event("fast", 9, 76, 0))
	if len(selector.quotaProjections) != 1 || !selector.quotaWindowExhausted(values[0]) {
		t.Fatalf("capacity lost latest reduction: %d", len(selector.quotaProjections))
	}
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "fast", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	baseCalls, _ = repo.callCounts("model-a")
	if baseCalls != 2 {
		t.Fatalf("capacity overflow did not invalidate stale pool: %d", baseCalls)
	}
}

func TestRedisQuotaProjectionPreservesSQLRevision(t *testing.T) {
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
	cfg := redisruntime.Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: databaseNumber, KeyPrefix: fmt.Sprintf("d11-quota:%d:", time.Now().UnixNano()), ConcurrencyLease: time.Minute}
	cleanup := redisclient.NewClient(&redisclient.Options{Addr: address, Username: cfg.Username, Password: cfg.Password, DB: databaseNumber})
	defer cleanup.Close()
	defer func() {
		cleanCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		iterator := cleanup.Scan(cleanCtx, 0, cfg.KeyPrefix+"*", 100).Iterator()
		for iterator.Next(cleanCtx) {
			if err := cleanup.Del(cleanCtx, iterator.Val()).Err(); err != nil {
				t.Error(err)
			}
		}
		if err := iterator.Err(); err != nil {
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
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(db)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "redis-quota", SourceKey: "redis-quota", AuthType: account.AuthTypeSSO, EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: credential.ID, SyncedAt: time.Now().UTC(), Windows: []account.QuotaWindow{{Mode: "fast", Remaining: 3, Total: 3}}}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	values, err := selector.loadCandidates(ctx, account.ProviderWeb, 0, "grok", "fast", time.Now().UTC())
	if err != nil || len(values) != 1 {
		t.Fatalf("routing=%+v %v", values, err)
	}
	updates := make(chan repository.InvalidationEvent, 16)
	done := make(chan error, 1)
	go func() {
		done <- b.ListenInvalidations(ctx, func(_ context.Context, e repository.InvalidationEvent) error {
			selector.ApplyInvalidation(e)
			updates <- e
			return nil
		})
	}()
	defer func() { cancel(); <-done }()
	ready := repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: credential.ID, Quota: &account.QuotaProjection{Mode: "fast", SnapshotVersion: 1, Revision: 1, Remaining: 3}}
	for {
		if err := a.PublishInvalidation(ctx, ready); err != nil {
			t.Fatal(err)
		}
		select {
		case <-updates:
			goto listening
		case <-ctx.Done():
			t.Fatal("Redis subscription unavailable")
		case <-time.After(15 * time.Millisecond):
		}
	}
listening:
	var captured repository.InvalidationEvent
	accounts.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { captured = e })
	fact := account.QuotaConsumption{EventID: "redis-quota-event", AccountID: credential.ID, Mode: "fast", SnapshotVersion: 1, Units: 3}
	if receipt, err := accounts.ConsumeQuota(ctx, fact, time.Now().UTC()); err != nil || receipt.State != account.QuotaConsumptionApplied {
		t.Fatalf("consumption=%+v %v", receipt, err)
	}
	if captured.Quota == nil || captured.Quota.Revision != 2 {
		t.Fatalf("missing SQL revision: %+v", captured)
	}
	if err := a.PublishInvalidation(ctx, captured); err != nil {
		t.Fatal(err)
	}
	for !selector.quotaWindowExhausted(values[0]) {
		select {
		case <-updates:
		case <-ctx.Done():
			t.Fatal("remote selector missed consumption")
		}
	}
	// Redis has a newer transport revision for this later message. Its older SQL
	// revision still cannot undo the already observed numeric state.
	if err := a.PublishInvalidation(ctx, ready); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case e := <-updates:
			if e.Quota != nil && e.Quota.Revision == 1 {
				goto reordered
			}
		case <-ctx.Done():
			t.Fatal("reordered projection not received")
		}
	}
reordered:
	if !selector.quotaWindowExhausted(values[0]) || values[0].QuotaWindow.Remaining != 3 {
		t.Fatal("remote replay changed authoritative reduction or immutable snapshot")
	}
	if _, err := accounts.ConsumeQuota(ctx, fact, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := a.PublishInvalidation(ctx, captured); err != nil {
		t.Fatal(err)
	}
}
