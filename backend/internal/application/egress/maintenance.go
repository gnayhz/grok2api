package egress

import (
	"context"
	"errors"
	"time"
)

// RunSubscriptionMaintenance 同步到期订阅源。组合根(app/application.go)
// 分别调度本方法与 RunProbeMaintenance,两条 pass 相互独立。
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
