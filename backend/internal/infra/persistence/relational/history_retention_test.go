package relational

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type pausedRetentionRepository struct {
	repository.ResponseRepository
	entered chan struct{}
	resume  chan struct{}
	calls   int
}

func (p *pausedRetentionRepository) DeleteExpired(ctx context.Context, now time.Time, owners, web int) (repository.ResponseCleanupResult, error) {
	p.calls++
	if p.calls == 1 {
		close(p.entered)
		select {
		case <-p.resume:
		case <-ctx.Done():
			return repository.ResponseCleanupResult{}, ctx.Err()
		}
	}
	return p.ResponseRepository.DeleteExpired(ctx, now, owners, web)
}
func TestHistoryRetentionRealStoresAndSharedLock(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			a, b := settingsDatabasePair(t, dialect)
			owner, _, err := NewAccountRepository(a).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "retention", SourceKey: "retention", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			key, err := NewClientKeyRepository(a).Create(ctx, clientkey.Key{Name: "retention", Prefix: "retention", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			var owners []responseOwnershipModel
			var states []webResponseStateModel
			for i := 0; i < 1003; i++ {
				expires := now.Add(-time.Minute)
				if i == 1002 {
					expires = now.Add(time.Hour)
				}
				owners = append(owners, responseOwnershipModel{ResponseID: fmt.Sprintf("owned_%d", i), AccountID: owner.ID, ClientKeyID: key.ID, ModelRouteID: 42, Provider: string(owner.Provider), ExpiresAt: expires, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)})
			}
			for i := 0; i < 53; i++ {
				expires := now.Add(-time.Minute)
				if i == 52 {
					expires = now.Add(time.Hour)
				}
				states = append(states, webResponseStateModel{ResponseID: fmt.Sprintf("native_%d", i), AccountID: owner.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: `{"status":"completed"}`, Status: "completed", ExpiresAt: expires, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)})
			}
			if err = a.db.CreateInBatches(owners, 100).Error; err != nil {
				t.Fatal(err)
			}
			if err = a.db.CreateInBatches(states, 50).Error; err != nil {
				t.Fatal(err)
			}
			var lock repository.DistributedLock = memory.NewLockStore()
			if dialect == "postgres" {
				address := os.Getenv("TEST_REDIS_ADDRESS")
				if address == "" {
					t.Skip("isolated Redis required")
				}
				shared, err := redisruntime.Open(ctx, redisruntime.Config{Address: address, KeyPrefix: fmt.Sprintf("g35:%d:", time.Now().UnixNano())})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = shared.Close() })
				lock = redisruntime.NewLockStore(shared)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			paused := &pausedRetentionRepository{ResponseRepository: NewResponseRepository(a), entered: make(chan struct{}), resume: make(chan struct{})}
			first := historyapp.NewRetention(paused, nil, lock, logger)
			second := historyapp.NewRetention(NewResponseRepository(b), nil, lock, logger)
			done := make(chan error, 1)
			go func() { done <- first.CleanupResponses(ctx, now) }()
			<-paused.entered
			if err = second.CleanupResponses(ctx, now); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err = b.db.Model(&responseOwnershipModel{}).Count(&count).Error; err != nil || count != 1003 {
				t.Fatalf("lock competitor deleted rows: count=%d err=%v", count, err)
			}
			close(paused.resume)
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			if paused.calls != 2 {
				t.Fatalf("bounded batches=%d", paused.calls)
			}
			if err = b.db.Model(&responseOwnershipModel{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("remaining ownership=%d err=%v", count, err)
			}
			if err = b.db.Model(&webResponseStateModel{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("remaining native=%d err=%v", count, err)
			}
			release, acquired, err := lock.Acquire(ctx, "response-ownership-cleanup", time.Minute)
			if err != nil || !acquired {
				t.Fatal("retention lock not released")
			}
			release()
			cipher, err := security.NewVersionedCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", nil)
			if err != nil {
				t.Fatal(err)
			}
			journal := NewConversationJournal(a, cipher, 8<<20)
			for _, name := range []string{"expired", "active_writer", "current"} {
				p := journalParams(name, "input")
				p.Scope.Key = name
				p.Now = now.Add(-2 * time.Hour)
				p.Retention = time.Hour
				p.Lease = 3 * time.Hour
				if name == "current" {
					p.Now = now
				}
				reservation, err := journal.Reserve(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				if name == "expired" {
					if err = journal.Release(ctx, reservation.Ticket); err != nil {
						t.Fatal(err)
					}
				}
			}
			maintenance := historyapp.NewRetention(NewResponseRepository(b), NewConversationJournal(b, cipher, 8<<20), lock, logger)
			if err = maintenance.CleanupConversations(ctx, now.In(time.FixedZone("positive", 8*3600))); err != nil {
				t.Fatal(err)
			}
			var sessions []conversationSessionModel
			if err = b.db.Find(&sessions).Error; err != nil {
				t.Fatal(err)
			}
			if len(sessions) != 2 {
				t.Fatalf("cleanup removed live history: %+v", sessions)
			}
			expired := journalScopeID(repository.JournalScope{Key: "expired", Model: "model", Normalizer: 1})
			for _, s := range sessions {
				if s.ID == expired {
					t.Fatal("expired history remains")
				}
			}
			if err = maintenance.CleanupConversations(ctx, now.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err = b.db.Model(&conversationSessionModel{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("expired writer session not collected: %d %v", count, err)
			}
		})
	}
}
