package court

import (
	"context"
	"sync"
)

// evaluationLock serializes case mutations while letting queued callers honor
// their deadlines. Its zero value is ready to use, like sync.Mutex.
type evaluationLock struct {
	once sync.Once
	busy chan struct{}
}

func (m *evaluationLock) Lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.once.Do(func() { m.busy = make(chan struct{}, 1) })
	select {
	case m.busy <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *evaluationLock) Unlock() { <-m.busy }
