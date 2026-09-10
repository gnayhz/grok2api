package egress

import (
	"context"
	"errors"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func (s *Service) ListNodeFacts(ctx context.Context) ([]domain.NodeFacts, error) {
	source, ok := s.repository.(repository.EgressNodeFactsReader)
	if !ok {
		return nil, errors.New("egress node facts unavailable")
	}
	return source.ListNodeFacts(ctx)
}

func (s *Service) NodeFacts(ctx context.Context, id uint64) (domain.NodeFacts, bool, error) {
	source, ok := s.repository.(repository.EgressNodeFactsReader)
	if !ok {
		return domain.NodeFacts{}, false, errors.New("egress node facts unavailable")
	}
	return source.NodeFacts(ctx, id)
}
