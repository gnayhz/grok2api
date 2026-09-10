package registry

import (
	"context"
	"time"
)

// The task adapter exposes maintenance with the same lease predicates used by
// settlement and registry recovery; composition does not choose queue policy.
func (s *ProbeTaskStore) ReclaimRunningProbes(ctx context.Context, reason string) (int64, error) {
	return s.registry.ReclaimRunningProbes(ctx, reason)
}

func (s *ProbeTaskStore) ReclaimStaleRunningProbes(ctx context.Context, staleAfter time.Duration) (int64, error) {
	return s.registry.ReclaimStaleRunningProbes(ctx, staleAfter)
}

func (s *ProbeTaskStore) CancelOrphanProbes(ctx context.Context, reason string) (int64, error) {
	return s.registry.CancelOrphanProbes(ctx, reason)
}
