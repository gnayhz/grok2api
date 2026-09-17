package relational

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/tokenhash"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	invalidationapp "github.com/chenyme/grok2api/backend/internal/application/invalidation"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

type remoteAuthReadGate struct {
	repository.ClientKeyRepository
	read, resume chan struct{}
	held         atomic.Bool
}

func (r *remoteAuthReadGate) GetByPrefix(ctx context.Context, prefix string) (clientkey.Key, error) {
	key, err := r.ClientKeyRepository.GetByPrefix(ctx, prefix)
	if r.held.CompareAndSwap(false, true) {
		close(r.read)
		select {
		case <-r.resume:
		case <-ctx.Done():
			return clientkey.Key{}, ctx.Err()
		}
	}
	return key, err
}

func authenticateRemoteKey(s *clientkeyapp.Service, ctx context.Context, raw string) (clientkey.Key, error) {
	key, release, err := s.Authenticate(ctx, raw)
	if release != nil {
		release()
	}
	return key, err
}

func TestClientKeyRedisInvalidationFencesSQLReads(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("requires isolated TEST_REDIS_ADDRESS")
	}
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, mutation := range []string{"disable", "restrict_empty", "create_inflight", "create_cached"} {
			t.Run(dialect+"/"+mutation, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				prefix := fmt.Sprintf("g15-auth-%d:", time.Now().UnixNano())
				store, err := redisruntime.Open(ctx, redisruntime.Config{Address: address, KeyPrefix: prefix})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				defer func() {
					client := redisclient.NewClient(&redisclient.Options{Addr: address})
					defer client.Close()
					cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if err := client.Del(cleanup, prefix+"invalidation-revision:client_key:").Err(); err != nil {
						t.Error(err)
					}
				}()
				keysA := NewClientKeyRepository(a)
				held := &remoteAuthReadGate{ClientKeyRepository: NewClientKeyRepository(b), read: make(chan struct{}), resume: make(chan struct{})}
				resume := sync.OnceFunc(func() { close(held.resume) })
				defer resume()
				serviceA := clientkeyapp.NewService("remote-test", keysA, nil, nil, 0, 0, nil, security.RandomTokenSource{})
				serviceB := clientkeyapp.NewService("remote-test", held, nil, nil, 0, 0, nil, security.RandomTokenSource{})
				defer func() {
					closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if err := serviceB.Close(closeCtx); err != nil {
						t.Error(err)
					}
					if err := serviceA.Close(closeCtx); err != nil {
						t.Error(err)
					}
				}()
				seen := make(chan repository.InvalidationEvent, 16)
				busA := invalidationapp.NewService(store, "instance-a", serviceA.ApplyInvalidation, nil)
				busB := invalidationapp.NewService(store, "instance-b", func(event repository.InvalidationEvent) {
					serviceB.ApplyInvalidation(event)
					select {
					case seen <- event:
					case <-ctx.Done():
					}
				}, nil)
				keysA.SetInvalidationObserver(busA.Notify)
				runCtx, stop := context.WithCancel(ctx)
				publisher, subscriber := make(chan error, 1), make(chan error, 1)
				go func() { publisher <- busA.RunPublisher(runCtx) }()
				go func() { subscriber <- busB.RunSubscriber(runCtx) }()
				defer func() {
					stop()
					if err := <-subscriber; err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
					if err := <-publisher; err != nil {
						t.Error(err)
					}
				}()
				// Confirm the actual subscriber has received a marker, without assuming
				// subscription from a fixed sleep or re-sending the mutation itself.
				ready := repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged, ClientKeyID: 999999, SourceInstance: "readiness"}
			readyLoop:
				for {
					if err := store.PublishInvalidation(ctx, ready); err != nil {
						t.Fatal(err)
					}
					select {
					case <-seen:
						break readyLoop
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				raw := clientkey.FormatClientKey("abcdef123456", "synthetic-key")
				seed := clientkey.Key{Name: "remote", Prefix: "abcdef123456", SecretHash: tokenhash.HashToken(raw), EncryptedSecret: "synthetic", Enabled: true}
				var key clientkey.Key
				waitForKey := func(id uint64) repository.InvalidationEvent {
					for {
						select {
						case event := <-seen:
							if event.ClientKeyID == id {
								return event
							}
						case <-ctx.Done():
							t.Fatal("committed key invalidation was not received", ctx.Err())
						}
					}
				}
				if mutation == "disable" || mutation == "restrict_empty" {
					key, err = keysA.Create(ctx, seed)
					if err != nil {
						t.Fatal(err)
					}
					waitForKey(key.ID) // The initial creation must precede the held read.
				}
				oldDone := make(chan error, 1)
				go func() { _, err := authenticateRemoteKey(serviceB, ctx, raw); oldDone <- err }()
				select {
				case <-held.read:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if mutation == "create_cached" {
					resume()
					if err := <-oldDone; !errors.Is(err, clientkeyapp.ErrInvalidKey) {
						t.Fatal(err)
					}
				}
				switch mutation {
				case "disable":
					disabled := false
					_, err = serviceA.Update(ctx, key.ID, clientkeyapp.UpdateInput{Enabled: &disabled})
				case "restrict_empty":
					restricted := clientkey.ModelScopeRestricted
					_, err = serviceA.Update(ctx, key.ID, clientkeyapp.UpdateInput{ModelScope: &restricted})
				default:
					key, err = keysA.Create(ctx, seed)
				}
				if err != nil {
					t.Fatal(err)
				}
				received := waitForKey(key.ID)
				if received.SourceInstance != "instance-a" || received.Revision == 0 {
					t.Fatalf("mutation did not cross actual publisher/subscriber: %+v", received)
				}
				resume()
				if mutation != "create_cached" {
					oldErr := <-oldDone
					if mutation == "create_inflight" && !errors.Is(oldErr, clientkeyapp.ErrInvalidKey) {
						t.Fatal(oldErr)
					}
					if mutation != "create_inflight" && oldErr != nil {
						t.Fatal(oldErr)
					}
				}
				check := func() {
					value, err := authenticateRemoteKey(serviceB, ctx, raw)
					switch mutation {
					case "disable":
						if !errors.Is(err, clientkeyapp.ErrInvalidKey) {
							t.Fatalf("received revocation was overwritten: %v", err)
						}
					case "restrict_empty":
						if err != nil || value.ModelScope != clientkey.ModelScopeRestricted || value.AllowsModel(1) {
							t.Fatalf("received model restriction was overwritten: %q %v", value.ModelScope, err)
						}
					default:
						if err != nil || value.ID != key.ID {
							t.Fatalf("creation hidden after its notification: %v", err)
						}
					}
				}
				check()
				// An old/duplicate notification only causes another reload; it never
				// supplies policy or rolls the cache back to the event's old state.
				serviceB.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged, ClientKeyID: key.ID, Revision: 1})
				check()
			})
		}
	}
}

func TestClientKeyCreateInvalidationOnlyAfterCommit(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			keys, peer := NewClientKeyRepository(a), NewClientKeyRepository(b)
			var seen []repository.InvalidationEvent
			keys.SetInvalidationObserver(func(ctx context.Context, event repository.InvalidationEvent) {
				seen = append(seen, event)
				key, err := peer.Get(ctx, event.ClientKeyID)
				if err != nil || key.ModelScope != clientkey.ModelScopeRestricted || len(key.AllowedModels) != 0 {
					t.Errorf("creation notification preceded complete commit: scope=%q, err=%v", key.ModelScope, err)
				}
			})
			seed := clientkey.Key{Name: "created", Prefix: "creation", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ModelScope: clientkey.ModelScopeRestricted}
			key, err := keys.Create(context.Background(), seed)
			if err != nil || len(seen) != 1 || seen[0].ClientKeyID != key.ID || !seen[0].Valid() {
				t.Fatalf("creation notification: count=%d, err=%v", len(seen), err)
			}
			seed.Prefix = "failed-creation"
			seed.AllowedModels = []uint64{999}
			if _, err := keys.Create(context.Background(), seed); !errors.Is(err, repository.ErrInvalidRecord) {
				t.Fatalf("invalid creation: %v", err)
			}
			if len(seen) != 1 {
				t.Fatal("rolled-back creation emitted a notification")
			}
		})
	}
}
