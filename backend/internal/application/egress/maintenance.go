package egress

import (
	"context"
	"errors"
	"time"
)

// RunMaintenance performs the background subscription sync and node probe
// passes. Egress scheduling is pure request-time routing: no account
// assignment cycle is needed.
func (s *Service) RunMaintenance(ctx context.Context) error {
	operations, err := s.operationsRepository()
	if err != nil {
		return err
	}
	config, err := operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return err
	}
	var resultErr error
	sources, err := operations.ListDueEgressSources(ctx, time.Now().UTC(), 3)
	if err != nil {
		resultErr = errors.Join(resultErr, err)
	} else {
		for _, source := range sources {
			if _, syncErr := s.syncSource(ctx, operations, source); syncErr != nil {
				resultErr = errors.Join(resultErr, syncErr)
			}
		}
	}
	nodes, err := operations.ListDueEgressNodes(ctx, time.Now().UTC(), time.Duration(config.ProbeIntervalSeconds)*time.Second, 32)
	if err != nil {
		resultErr = errors.Join(resultErr, err)
	} else if len(nodes) > 0 {
		ids := make([]uint64, 0, len(nodes))
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		if _, probeErr := s.TestNodes(ctx, ids); probeErr != nil {
			resultErr = errors.Join(resultErr, probeErr)
		}
	}
	return resultErr
}
