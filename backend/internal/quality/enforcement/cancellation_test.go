package enforcement

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"testing"
	"time"
)

type blockedNodeSource struct{ started chan struct{} }

func (s blockedNodeSource) ListProfiles(ctx context.Context) ([]proxy.NodeProfile, error) {
	close(s.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s blockedNodeSource) Profile(ctx context.Context, _ uint64) (proxy.NodeProfile, bool, error) {
	<-ctx.Done()
	return proxy.NodeProfile{}, false, ctx.Err()
}
func TestCloseCancelsBlockedInitialNodeRead(t *testing.T) {
	registry, _ := newBench(t)
	started := make(chan struct{})
	service := New(DefaultConfig(), registry, blockedNodeSource{started}, memIPSource{}, newMemRotator())
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
