package app

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/quality/events"
	"time"
)

// qualityEventSink adapts the Gateway receipt and backlog DTOs. All receipt
// classification and asynchronous incident processing belong to M16 events.
type qualityEventSink struct{ *events.Service }

func (s *qualityEventSink) GuardBacklog(ctx context.Context) (gateway.GuardBacklogStats, error) {
	stats, err := s.Backlog(ctx)
	return gateway.GuardBacklogStats{InFlight: stats.InFlight, Unconfirmed: stats.Unconfirmed, OldestExpiredAt: stats.OldestExpiredAt, Pending: stats.Pending, Limit: stats.Limit, OldestAt: stats.OldestAt, Leased: stats.Leased, Retrying: stats.Retrying}, err
}
func (s *qualityEventSink) RecordQualityEvent(ctx context.Context, obs gateway.QualityObservation, ttl time.Duration) error {
	return s.Service.RecordQualityEvent(ctx, events.Receipt{Attempt: obs.Attempt, Outcome: events.Outcome(obs.Outcome), At: obs.At, Rule: obs.Rule, ErrorCode: obs.ErrorCode}, ttl)
}
