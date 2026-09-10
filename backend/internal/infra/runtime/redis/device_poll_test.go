package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func devicePollRuntimePair(t *testing.T, kind string) (repository.DeviceSessionRepository, repository.DeviceSessionRepository) {
	t.Helper()
	if kind == "memory" {
		store := memory.NewDeviceSessionStore()
		return store, store
	}
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS required")
	}
	ctx := context.Background()
	cfg := Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), KeyPrefix: fmt.Sprintf("g24-device-%d:", time.Now().UnixNano())}
	if raw := os.Getenv("TEST_REDIS_DATABASE"); raw != "" {
		var err error
		cfg.Database, err = strconv.Atoi(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	t.Cleanup(func() { cleanupRedisTestPrefix(t, ctx, first.client, cfg.KeyPrefix) })
	second, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	return NewDeviceSessionStore(first), NewDeviceSessionStore(second)
}

func TestDevicePollRuntimeClaimsAndFencesCompletion(t *testing.T) {
	for _, kind := range []string{"memory", "redis"} {
		t.Run(kind, func(t *testing.T) {
			first, second := devicePollRuntimePair(t, kind)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Millisecond)
			session := account.DeviceSession{ID: "race", DeviceCode: "synthetic", Interval: time.Second, ExpiresAt: now.Add(5 * time.Minute)}
			if err := first.Create(ctx, session); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			winners := make(chan string, 20)
			errs := make(chan error, 20)
			start := make(chan struct{})
			for i := range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					store := first
					if i%2 != 0 {
						store = second
					}
					token := fmt.Sprint(i)
					_, err := store.ClaimPoll(ctx, session.ID, token, now, now.Add(30*time.Second))
					if err == nil {
						winners <- token
					} else if !errors.Is(err, account.ErrDevicePollTooSoon) {
						errs <- err
					}
				}()
			}
			close(start)
			wg.Wait()
			close(winners)
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			var tokens []string
			for token := range winners {
				tokens = append(tokens, token)
			}
			if len(tokens) != 1 {
				t.Fatalf("due poll winners=%v", tokens)
			}
			receipt := account.DevicePollReceipt{SessionID: session.ID, Token: tokens[0]}
			if _, err := second.ClaimPoll(ctx, session.ID, "too-early", now.Add(2*time.Second), now.Add(time.Minute)); !errors.Is(err, account.ErrDevicePollTooSoon) {
				t.Fatalf("live owner lost exclusivity: %v", err)
			}
			if applied, err := second.FinishPoll(ctx, account.DevicePollReceipt{SessionID: session.ID, Token: "other"}, account.DevicePollCompletion{Kind: account.DevicePollDenied, CompletedAt: now}); err != nil || applied {
				t.Fatalf("wrong token completed: %t %v", applied, err)
			}
			event := account.DevicePollCompletion{Kind: account.DevicePollPending, CompletedAt: now.Add(2 * time.Second)}
			if applied, err := first.FinishPoll(ctx, receipt, event); err != nil || !applied {
				t.Fatalf("pending completion: %t %v", applied, err)
			}
			if applied, err := second.FinishPoll(ctx, receipt, event); err != nil || applied {
				t.Fatalf("duplicate completion applied: %t %v", applied, err)
			}
			stored, err := second.Get(ctx, session.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if stored.PollToken != "" || stored.Interval != time.Second || !stored.NextPollAt.Equal(now.Add(3*time.Second)) || !stored.ExpiresAt.Equal(session.ExpiresAt) {
				t.Fatal("pending transition lost time or expiry")
			}
			if _, err := first.ClaimPoll(ctx, session.ID, "early", now.Add(2500*time.Millisecond), now.Add(time.Minute)); !errors.Is(err, account.ErrDevicePollTooSoon) {
				t.Fatalf("next interval not enforced: %v", err)
			}
			next, err := second.ClaimPoll(ctx, session.ID, "second", now.Add(3*time.Second), now.Add(30*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if applied, err := first.FinishPoll(ctx, account.DevicePollReceipt{SessionID: session.ID, Token: next.PollToken}, account.DevicePollCompletion{Kind: account.DevicePollSlowDown, CompletedAt: now.Add(4 * time.Second)}); err != nil || !applied {
				t.Fatalf("slow-down completion: %t %v", applied, err)
			}
			if applied, err := second.FinishPoll(ctx, receipt, event); err != nil || applied {
				t.Fatalf("late old result applied: %t %v", applied, err)
			}
			stored, err = first.Get(ctx, session.ID, now)
			if err != nil || stored.Interval != 6*time.Second || !stored.NextPollAt.Equal(now.Add(10*time.Second)) {
				t.Fatalf("old result overwrote slow-down: %v", err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := second.ClaimPoll(canceled, session.ID, "canceled", now.Add(10*time.Second), now.Add(time.Minute)); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled claim=%v", err)
			}
			old, err := first.ClaimPoll(ctx, session.ID, "old-lease", now.Add(10*time.Second), now.Add(20*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := second.ClaimPoll(ctx, session.ID, "fresh-lease", now.Add(21*time.Second), now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if applied, err := first.FinishPoll(ctx, account.DevicePollReceipt{SessionID: session.ID, Token: old.PollToken}, account.DevicePollCompletion{Kind: account.DevicePollDenied, CompletedAt: now.Add(22 * time.Second)}); err != nil || applied {
				t.Fatalf("expired owner deleted current lease: %t %v", applied, err)
			}
			if applied, err := second.FinishPoll(ctx, account.DevicePollReceipt{SessionID: session.ID, Token: fresh.PollToken}, account.DevicePollCompletion{Kind: account.DevicePollAuthorized, CompletedAt: now.Add(23 * time.Second)}); err != nil || !applied {
				t.Fatalf("terminal result=%t %v", applied, err)
			}
			if _, err := first.Get(ctx, session.ID, now); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("terminal session remains: %v", err)
			}
			if applied, err := first.FinishPoll(ctx, receipt, event); err != nil || applied {
				t.Fatalf("old result recreated terminal session: %t %v", applied, err)
			}
		})
	}
}

func TestDevicePollRedisFailedCASDoesNotClaim(t *testing.T) {
	first, second := devicePollRuntimePair(t, "redis")
	ctx := context.Background()
	now := time.Now().UTC()
	session := account.DeviceSession{ID: "cas-failure", DeviceCode: "synthetic", Interval: time.Second, ExpiresAt: now.Add(time.Minute)}
	if err := first.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	store := first.(*DeviceSessionStore).store
	hook := &failOnceRedisCommandHook{}
	store.client.AddHook(hook)
	hook.arm("evalsha")
	if _, err := first.ClaimPoll(ctx, session.ID, "failed", now, now.Add(30*time.Second)); err == nil {
		t.Fatal("injected storage failure reported a claim")
	}
	stored, err := second.Get(ctx, session.ID, now)
	if err != nil || stored.PollToken != "" {
		t.Fatalf("failed CAS changed state: %v", err)
	}
	if _, err := second.ClaimPoll(ctx, session.ID, "retry", now, now.Add(30*time.Second)); err != nil {
		t.Fatalf("failed CAS blocked next valid claim: %v", err)
	}
}

func TestDevicePollExpiryKeepsOnlyClaimedCompletion(t *testing.T) {
	for _, kind := range []string{"memory", "redis"} {
		t.Run(kind, func(t *testing.T) {
			first, second := devicePollRuntimePair(t, kind)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Millisecond)
			session := account.DeviceSession{ID: "expiry", DeviceCode: "synthetic", Interval: time.Millisecond, ExpiresAt: now.Add(150 * time.Millisecond)}
			if err := first.Create(ctx, session); err != nil {
				t.Fatal(err)
			}
			claimed, err := first.ClaimPoll(ctx, session.ID, "owner", now, now.Add(2*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			// Observe actual key TTL expiry, not just an advanced logical clock.
			time.Sleep(time.Until(session.ExpiresAt.Add(20 * time.Millisecond)))
			if _, err := second.Get(ctx, session.ID, time.Now()); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("expired authorization remained visible: %v", err)
			}
			if _, err := second.ClaimPoll(ctx, session.ID, "late", time.Now(), time.Now().Add(time.Second)); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("expired authorization issued a new poll: %v", err)
			}
			if applied, err := first.FinishPoll(ctx, account.DevicePollReceipt{SessionID: session.ID, Token: claimed.PollToken}, account.DevicePollCompletion{Kind: account.DevicePollAuthorized, CompletedAt: time.Now()}); err != nil || !applied {
				t.Fatalf("expiry discarded active owner's completion: %t %v", applied, err)
			}
			if applied, err := second.FinishPoll(ctx, account.DevicePollReceipt{SessionID: session.ID, Token: claimed.PollToken}, account.DevicePollCompletion{Kind: account.DevicePollPending, CompletedAt: time.Now()}); err != nil || applied {
				t.Fatalf("terminal claim resurrected: %t %v", applied, err)
			}
		})
	}
}
