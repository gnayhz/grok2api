package invalidation

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type testBus struct {
	mu         sync.Mutex
	published  []repository.InvalidationEvent
	events     []repository.InvalidationEvent
	publishErr error
}

func (b *testBus) PublishInvalidation(_ context.Context, event repository.InvalidationEvent) error {
	b.mu.Lock()
	b.published = append(b.published, event)
	b.mu.Unlock()
	return b.publishErr
}

func (b *testBus) ListenInvalidations(ctx context.Context, handler func(context.Context, repository.InvalidationEvent) error) error {
	for _, event := range b.events {
		if err := handler(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func TestNotifyAlwaysAppliesLocalInvalidation(t *testing.T) {
	var applied []repository.InvalidationEvent
	service := NewService(nil, "local", func(event repository.InvalidationEvent) {
		applied = append(applied, event)
	}, slog.Default())
	service.Notify(context.Background(), repository.InvalidationEvent{
		Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild,
	})
	if len(applied) != 1 || applied[0].SourceInstance != "local" || applied[0].PublishedAt.IsZero() {
		t.Fatalf("local invalidation = %#v", applied)
	}
}

func TestNotifyAppliesLocallyWhenRemoteQueueIsFull(t *testing.T) {
	var applied int
	service := NewService(&testBus{}, "local", func(repository.InvalidationEvent) { applied++ }, slog.Default())
	for range cap(service.queue) {
		service.queue <- repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged}
	}
	service.Notify(context.Background(), repository.InvalidationEvent{Kind: repository.InvalidationAccountBillingChanged})
	if applied != 1 || service.dropped.Load() != 1 {
		t.Fatalf("applied=%d dropped=%d", applied, service.dropped.Load())
	}
}

func TestRunSubscriberIgnoresInvalidAndSameSourceEvents(t *testing.T) {
	bus := &testBus{events: []repository.InvalidationEvent{
		{Kind: "unknown", SourceInstance: "remote"},
		{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild, SourceInstance: "local"},
		{Kind: repository.InvalidationAccountQuotaChanged, Provider: account.ProviderWeb, SourceInstance: "remote", Revision: 2},
	}}
	var applied []repository.InvalidationEvent
	service := NewService(bus, "local", func(event repository.InvalidationEvent) {
		applied = append(applied, event)
	}, slog.Default())
	if err := service.RunSubscriber(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Kind != repository.InvalidationAccountQuotaChanged {
		t.Fatalf("applied events = %#v", applied)
	}
}

func TestRunPublisherDoesNotStopAfterPublishFailure(t *testing.T) {
	bus := &testBus{publishErr: errors.New("redis unavailable")}
	service := NewService(bus, "local", nil, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.RunPublisher(ctx) }()
	service.Notify(context.Background(), repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	deadline := time.Now().Add(time.Second)
	for service.failures.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if service.failures.Load() != 1 {
		t.Fatalf("publish failures = %d", service.failures.Load())
	}
}

func TestEventKeyKeepsClientKeyInvalidationsDistinct(t *testing.T) {
	first := eventKey(repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged, ClientKeyID: 1})
	second := eventKey(repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged, ClientKeyID: 2})
	global := eventKey(repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged})
	if first == second || first == global || second == global {
		t.Fatalf("client-key invalidation keys were coalesced: first=%#v second=%#v global=%#v", first, second, global)
	}
}

func TestEventKeyKeepsAccountHealthInvalidationsDistinct(t *testing.T) {
	first := eventKey(repository.InvalidationEvent{Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 1})
	second := eventKey(repository.InvalidationEvent{Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 2})
	if first == second {
		t.Fatalf("account health invalidations were coalesced: first=%#v second=%#v", first, second)
	}
}

func TestPublisherCoalescesQuotaByAccountModeAndSQLRevision(t *testing.T) {
	bus := &testBus{}
	service := NewService(bus, "quota-test", nil, nil)
	event := func(id uint64, mode string, revision uint64) repository.InvalidationEvent {
		return repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: id, Quota: &account.QuotaProjection{Mode: mode, SnapshotVersion: 1, Revision: revision, Remaining: 1}}
	}
	for _, value := range []repository.InvalidationEvent{event(1, "fast", 3), event(1, "fast", 2), event(1, "other", 2), event(2, "fast", 2), {Kind: repository.InvalidationAccountQuotaChanged}} {
		service.Notify(context.Background(), value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.RunPublisher(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		bus.mu.Lock()
		values := append([]repository.InvalidationEvent(nil), bus.published...)
		bus.mu.Unlock()
		if len(values) >= 4 {
			if len(values) != 4 {
				t.Fatalf("coalesced notifications=%+v", values)
			}
			found := false
			for _, value := range values {
				if value.Quota != nil && value.AccountID == 1 && value.Quota.Mode == "fast" {
					found = true
					if value.Quota.Revision != 3 {
						t.Fatalf("late old projection replaced new=%+v", value)
					}
				}
			}
			if !found {
				t.Fatal("quota event disappeared")
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("publisher incomplete=%+v", values)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestPublisherCoalescesHealthBySQLRevision(t *testing.T) {
	bus := &testBus{}
	service := NewService(bus, "health-test", nil, nil)
	for _, revision := range []uint64{3, 2, 0} {
		service.Notify(context.Background(), repository.InvalidationEvent{Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 1, HealthRevision: revision})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	done := make(chan error, 1)
	go func() { done <- service.RunPublisher(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		bus.mu.Lock()
		values := append([]repository.InvalidationEvent(nil), bus.published...)
		bus.mu.Unlock()
		if len(values) > 0 {
			if len(values) != 1 || values[0].HealthRevision != 3 {
				t.Fatalf("coalesced health=%+v", values)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("publisher timed out")
		case <-time.After(time.Millisecond):
		}
	}
}
