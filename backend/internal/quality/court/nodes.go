package court

import (
	"context"
	"errors"
	"time"

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

// A comparison must be usable now even when its historical evidence is clean.
// This is a measurement-control constraint: it does not change transport's
// independent verification bypass or rotating-endpoint routing policy.
func (s *Service) comparisonNodes(ctx context.Context) (map[uint64]bool, error) {
	source, err := s.nodeSource()
	if err != nil {
		return nil, err
	}
	profiles, err := source.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	nodes := make(map[uint64]bool, len(profiles))
	for _, node := range profiles {
		if node.ID != 0 && node.Enabled && node.CanServeFixedTarget && (node.CooldownUntil == nil || !now.Before(*node.CooldownUntil)) {
			nodes[node.ID] = true
		}
	}
	return nodes, nil
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
