package egress

import (
	"context"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// qualityStubRepo 覆盖守卫/轮换路径需要的仓储方法。
type qualityStubRepo struct {
	ServiceRepository
	mu    sync.Mutex
	nodes map[uint64]domain.Node
}

func (r *qualityStubRepo) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	node, ok := r.nodes[id]
	if !ok {
		return domain.Node{}, repository.ErrNotFound
	}
	return node, nil
}

func (r *qualityStubRepo) ListEgressNodes(_ context.Context, _ repository.SortQuery) ([]domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var nodes []domain.Node
	for _, node := range r.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// fakeQuarantiner 记录死出口传输冷却调用(旧质量隔离方法已删除,仅保留
// CooldownNodeForProbeFailure 语义)。
type fakeQuarantiner struct {
	mu            sync.Mutex
	probeCooldown []uint64
}

func (f *fakeQuarantiner) CooldownNodeForProbeFailure(_ context.Context, nodeID uint64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCooldown = append(f.probeCooldown, nodeID)
	return nil
}
