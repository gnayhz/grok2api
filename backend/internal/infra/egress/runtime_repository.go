package egress

import (
	"context"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type runtimeNodeRepository interface {
	GetRuntimeEgressNode(context.Context, uint64) (domain.Node, error)
	ListRuntimeEgressNodes(context.Context) ([]domain.Node, error)
	ListRuntimeEgressPoolNodes(context.Context, uint64) ([]domain.Node, error)
}

func (m *routingRuntime) getRuntimeNode(ctx context.Context, id uint64) (domain.Node, error) {
	if store, ok := m.repository.(runtimeNodeRepository); ok {
		return store.GetRuntimeEgressNode(ctx, id)
	}
	return m.repository.GetEgressNode(ctx, id)
}

func (m *routingRuntime) listRuntimeNodes(ctx context.Context) ([]domain.Node, error) {
	if store, ok := m.repository.(runtimeNodeRepository); ok {
		return store.ListRuntimeEgressNodes(ctx)
	}
	return m.repository.ListEgressNodes(ctx, repository.SortQuery{})
}
