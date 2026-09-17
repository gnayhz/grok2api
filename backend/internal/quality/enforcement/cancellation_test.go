package enforcement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

type blockedNodeSource struct{ started chan struct{} }

func (s blockedNodeSource) ListProfiles(ctx context.Context) ([]proxy.NodeProfile, error) {
	close(s.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type delayedCloseNodes struct {
	blockedNodeSource
	release chan struct{}
}

func (s delayedCloseNodes) ListProfiles(ctx context.Context) ([]proxy.NodeProfile, error) {
	close(s.started)
	<-ctx.Done()
	<-s.release
	return nil, ctx.Err()
}

func TestRepeatedCloseWaitsForPollAfterTimeout(t *testing.T) {
	registry, _ := newBench(t)
	source := delayedCloseNodes{blockedNodeSource: blockedNodeSource{started: make(chan struct{})}, release: make(chan struct{})}
	service := New(DefaultConfig(), registry, source, memIPSource{}, newMemRotator())
	service.Run(context.Background())
	defer service.Close(context.Background())
	defer close(source.release)
	select {
	case <-source.started:
	case <-time.After(3 * time.Second):
		t.Fatal("poll did not start")
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := service.Close(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("close %d returned %v while poll still owns its dependency", i+1, err)
		}
	}
}
func (s blockedNodeSource) Profile(ctx context.Context, _ uint64) (proxy.NodeProfile, bool, error) {
	<-ctx.Done()
	return proxy.NodeProfile{}, false, ctx.Err()
}
func TestCloseCancelsBlockedInitialNodeRead(t *testing.T) {
	registry, _ := newBench(t)
	started := make(chan struct{})
	service := New(DefaultConfig(), registry, blockedNodeSource{started}, memIPSource{}, newMemRotator())
	go service.Run(context.Background())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("poll never started")
	}
	done := make(chan struct{})
	go func() { service.Close(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel active database read")
	}
	service.Close(context.Background())
}
