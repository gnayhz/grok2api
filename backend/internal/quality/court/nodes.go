package court

import (
	"context"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

func (s *Service) SetNodes(source proxy.NodeSource) {
	s.mu.Lock()
	s.nodes = source
	s.mu.Unlock()
}

func (s *Service) nodeSource() (proxy.NodeSource, error) {
	s.mu.RLock()
	source := s.nodes
	s.mu.RUnlock()
	if source == nil {
		return nil, errors.New("quality node facts unavailable")
	}
	return source, nil
}

// Only an exit-guilty decision needs the transport type. Fixed and webhook
// exits may be banned; a pool remains held until its epoch changes. Missing
// facts never supply a default type.
func (s *Service) exitBanApplicable(ctx context.Context, nodeID uint64) (ban, found bool, err error) {
	source, err := s.nodeSource()
	if err != nil {
		return false, false, err
	}
	node, found, err := source.Profile(ctx, nodeID)
	return !node.ProxyPool, found, err
}
