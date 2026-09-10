package egress

import (
	"context"
	"errors"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"time"
)

func (m *Manager) listNodes(ctx context.Context, now time.Time) ([]domain.Node, error) {
	return m.routing.listNodes(ctx, now)
}
func (m *Manager) invalidateNodes()            { m.routing.invalidateNodes() }
func (m *Manager) InvalidateNodeSnapshots()    { m.routing.invalidateNodes() }
func (m *Manager) InvalidateOperationsConfig() { m.routing.InvalidateOperationsConfig() }
func (m *Manager) InvalidatePoolCache()        { m.routing.InvalidatePoolCache() }
func (m *Manager) cachedRoutingTargetNode(ctx context.Context, id uint64) (domain.Node, bool, error) {
	return m.routing.cachedRoutingTargetNode(ctx, id)
}
func (m *Manager) AcquirePoolRouted(ctx context.Context, scope domain.Scope, affinity string, poolID uint64, allowDirect bool, cookies string) (*Lease, PoolRouteOutcome, error) {
	for attempt := 0; attempt < clientCreationRetryLimit; attempt++ {
		lease, outcome, err := m.routing.AcquirePoolRouted(ctx, scope, affinity, poolID, allowDirect, cookies)
		if !errors.Is(err, errNodeSnapshotInvalidated) {
			return lease, outcome, err
		}
	}
	return nil, PoolRouteNone, errNodeSnapshotInvalidated
}
func (m *Manager) getRuntimeNode(ctx context.Context, id uint64) (domain.Node, error) {
	return m.routing.getRuntimeNode(ctx, id)
}
func (m *Manager) listRuntimeNodes(ctx context.Context) ([]domain.Node, error) {
	return m.routing.listRuntimeNodes(ctx)
}
func (m *Manager) poolCandidates(ctx context.Context, nodes []domain.Node, now time.Time) []domain.Node {
	return m.routing.poolCandidates(ctx, nodes, now)
}
func (m *Manager) cachedNodeIsHealthy(id uint64) bool { return m.routing.cachedNodeIsHealthy(id) }
