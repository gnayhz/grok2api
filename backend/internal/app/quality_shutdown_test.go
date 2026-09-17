package app

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/quality/enforcement"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

type shutdownRuntime struct {
	io.Closer
	closed atomic.Bool
}

func (r *shutdownRuntime) Close() error {
	r.closed.Store(true)
	if r.Closer != nil {
		return r.Closer.Close()
	}
	return nil
}

type shutdownNodeSource struct {
	started  chan struct{}
	finished chan error
	inspect  func() error
}

func (s shutdownNodeSource) ListProfiles(ctx context.Context) ([]proxy.NodeProfile, error) {
	close(s.started)
	<-ctx.Done()
	s.finished <- s.inspect()
	return nil, ctx.Err()
}
func (shutdownNodeSource) Profile(context.Context, uint64) (proxy.NodeProfile, bool, error) {
	return proxy.NodeProfile{}, false, nil
}
func (shutdownNodeSource) CurrentExitIdentity(context.Context, uint64) (model.ExitIdentity, uint64, bool, error) {
	return model.ExitIdentity{}, 0, false, nil
}

func TestApplicationCloseStopsQualityBeforeRuntimeAndSQL(t *testing.T) {
	a := newLifecycleApplication(t)
	if err := a.qualityEnforcement.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime := &shutdownRuntime{Closer: a.runtime}
	a.runtime = runtime
	source := shutdownNodeSource{started: make(chan struct{}), finished: make(chan error, 1), inspect: func() error {
		if runtime.closed.Load() {
			return errors.New("runtime closed under the quality worker")
		}
		_, _, err := relational.NewAuditRepository(a.database).List(context.Background(), 0, 1)
		return err
	}}
	cfg := enforcement.DefaultConfig()
	cfg.Logger = a.logger
	a.qualityEnforcement = enforcement.New(cfg, a.quality, source, source, nil)
	a.qualityEnforcement.Run(context.Background())
	<-source.started
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-source.finished; err != nil {
		t.Fatal(err)
	}
	if !runtime.closed.Load() {
		t.Fatal("runtime was not closed after the quality worker stopped")
	}
	if a.database.Stats().OpenConnections != 0 {
		t.Fatal("SQL connections remained after Close")
	}
}
