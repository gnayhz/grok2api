package egress

import (
	"context"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

// taskRuntime owns maintenance and shared work. Categories have independent
// concurrency limits so a slow solver cannot consume probe or storage slots.
// Admission happens before spawning; overload cannot build a goroutine queue.
type taskRuntime struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	active  map[string]int
	limits  map[string]int
	changed chan struct{}
	closed  bool
	dropped uint64
}

func newTaskRuntime() *taskRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &taskRuntime{ctx: ctx, cancel: cancel, active: make(map[string]int), limits: map[string]int{"load": 64, "client": 64, "clearance": 8, "probe": 8, "failure_probe": 8, "rotation": 8, "idle_sweep": 1, "refresh": 8}, changed: make(chan struct{})}
}
func (t *taskRuntime) begin(kind string) (func(), error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, netbudget.ErrClosed
	}
	limit := t.limits[kind]
	if limit == 0 {
		limit = 8
	}
	if t.active[kind] >= limit {
		t.dropped++
		t.mu.Unlock()
		return nil, netbudget.ErrCapacity
	}
	t.active[kind]++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			t.active[kind]--
			close(t.changed)
			t.changed = make(chan struct{})
			t.mu.Unlock()
		})
	}, nil
}
func (t *taskRuntime) start(kind string, fn func()) bool {
	done, err := t.begin(kind)
	if err != nil {
		return false
	}
	go func() { defer done(); fn() }()
	return true
}
func (t *taskRuntime) stop() {
	t.mu.Lock()
	t.closed = true
	t.cancel()
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
}
func (t *taskRuntime) wait(ctx context.Context) error {
	for {
		t.mu.Lock()
		n := 0
		for _, v := range t.active {
			n += v
		}
		changed := t.changed
		t.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (t *taskRuntime) context(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	stop := context.AfterFunc(t.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (m *Manager) sharedLoad(ctx context.Context, group *sharedLoadGroup, key string, load func() (any, error)) (any, error) {
	waitDone, err := m.transport.network.BeginWait()
	if err != nil {
		return nil, err
	}
	defer waitDone()
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.tasks.ctx, cancel)
	defer stop()
	return waitSharedLoad(waitCtx, group, key, func() (any, error) {
		done, err := m.tasks.begin("load")
		if err != nil {
			return nil, err
		}
		defer done()
		return load()
	})
}
