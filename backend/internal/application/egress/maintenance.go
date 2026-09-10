package egress

import (
	"context"
	"errors"
	"time"
)

// RunMaintenance keeps the compatibility entry while running its independent
// passes concurrently. A slow subscription cannot delay a due health probe.
func (s *Service) RunMaintenance(ctx context.Context) error {
	results := make(chan error, 2)
	go func() { results <- s.RunSubscriptionMaintenance(ctx) }()
	go func() { results <- s.RunProbeMaintenance(ctx) }()
	return errors.Join(<-results, <-results)
}
func (s *Service) RunSubscriptionMaintenance(ctx context.Context) error {
	if !s.subscriptionMaintenance.TryLock() {
		return nil
	}
	defer s.subscriptionMaintenance.Unlock()
	operations, err := s.operationsRepository()
	if err != nil {
		return err
	}
	sources, err := operations.ListDueEgressSources(ctx, time.Now().UTC(), 3)
	if err != nil {
		return err
	}
	var result error
	for _, source := range sources {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		_, err := s.syncSource(ctx, operations, source)
		result = errors.Join(result, err)
	}
	return result
}
func (s *Service) RunProbeMaintenance(ctx context.Context) error {
	if !s.probeMaintenance.TryLock() {
		return nil
	}
	defer s.probeMaintenance.Unlock()
	operations, err := s.operationsRepository()
	if err != nil {
		return err
	}
	config, err := operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return err
	}
	nodes, err := operations.ListDueEgressNodes(ctx, time.Now().UTC(), time.Duration(config.ProbeIntervalSeconds)*time.Second, 32)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	_, err = s.TestNodes(ctx, ids)
	return err
}
