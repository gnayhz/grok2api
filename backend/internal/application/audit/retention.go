package audit

import (
	"context"
	"errors"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// RetentionPolicySource must read durable intent for every batch. Failed reads
// stop this sweep; cached policy is never a fallback for destructive work.
type RetentionPolicySource interface {
	AuditRetentionPolicy(context.Context) (auditdomain.RetentionPolicy, error)
}

const auditRetentionBatchSize = 500

// RunRetention owns the only periodic audit retention loop, including when the
// current policy is zero. That permits later updates without restarting workers.
func (s *Service) RunRetention(ctx context.Context, source RetentionPolicySource) error {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		deleted, err := s.SweepRetention(ctx, source)
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("audit_retention_sweep_failed", "error", err, "deleted", deleted)
		} else if deleted > 0 {
			s.logger.Info("audit_retention_swept", "deleted", deleted)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// SweepRetention bounds a round to 30 seconds and each transaction to 500 rows.
// A policy change takes effect before the next batch; an already started
// transaction may finish under the policy it read before that change.
func (s *Service) SweepRetention(ctx context.Context, source RetentionPolicySource) (int, error) {
	return s.sweepRetention(ctx, source, 30*time.Second)
}

func (s *Service) sweepRetention(ctx context.Context, source RetentionPolicySource, budget time.Duration) (int, error) {
	if source == nil {
		return 0, errors.New("audit retention policy source is required")
	}
	roundCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	total := 0
	for {
		if err := roundCtx.Err(); err != nil {
			return total, err
		}
		policy, err := source.AuditRetentionPolicy(roundCtx)
		if err != nil {
			return total, err
		}
		if err := policy.Validate(); err != nil {
			return total, err
		}
		cutoff, enabled := policy.Cutoff(s.now())
		if !enabled {
			return total, nil
		}
		deleted, err := s.audits.DeleteOlderThan(roundCtx, cutoff, auditRetentionBatchSize)
		total += deleted
		if err != nil || deleted < auditRetentionBatchSize {
			return total, err
		}
	}
}
