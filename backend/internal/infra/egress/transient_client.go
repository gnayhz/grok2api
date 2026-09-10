package egress

import (
	"context"
	"sync"
)

// transientClient owns a non-cached probe/download client in the same registry
// as provider clients. Its caller releases the lease after response completion;
// shutdown can still find the owner and cancel its requests and sockets.
func (m *clientRegistry) transientClient(ctx context.Context, factory func() (requestClient, error)) (*clientHandle, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	release, err := m.network.TryClient()
	if err != nil {
		return nil, nil, err
	}
	client, err := factory()
	if err != nil {
		release()
		return nil, nil, err
	}
	h := &clientHandle{registry: m, client: client, releaseBudget: release, leases: 1}
	m.owned.Store(client, h)
	var once sync.Once
	closeOwner := func() { once.Do(func() { h.retire(); h.releaseLease() }) }
	if m.closed.Load() {
		closeOwner()
		return nil, nil, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		closeOwner()
		return nil, nil, err
	}
	return h, closeOwner, nil
}
