package gateway

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// Quality collaborators consume immutable execution facts. Authoritative
// receipts are durable; optional telemetry may drop observations under pressure.

// QualityObservationOutcome 是一次守卫判决的观测结果。
type QualityObservationOutcome string

const (
	// Admission satisfies the configured protocol rule only. The wire value
	// "delivered" is retained for journal compatibility, not a completion claim.
	QualityObservedAdmitted    QualityObservationOutcome = "delivered"
	QualityObservedCompleted   QualityObservationOutcome = "completed"
	QualityObservedInterrupted QualityObservationOutcome = "interrupted"
	QualityObservedCanceled    QualityObservationOutcome = "canceled"
	// QualityObservedDegraded 降智(扣留,不达客户端)。
	QualityObservedDegraded QualityObservationOutcome = "degraded"
	QualityObservedRejected QualityObservationOutcome = "rejected"
)

// QualityObservation retains the immutable attempt and its observed outcome.
// The recorder must not reconstruct an exit epoch from current routing state.
type QualityObservation struct {
	Attempt   attemptmeta.Identity
	At        time.Time
	ErrorCode string
	AccountID uint64
	NodeID    uint64
	Provider  string
	Outcome   QualityObservationOutcome
	// Rule 是守卫规则指纹(脱敏,不含地址/账号名)。
	Rule string
}

// QualityEventRecorder returns only after durable acceptance. Unlike the
// optional telemetry observer, this seam must not discard authoritative facts.
type QualityEventRecorder interface {
	RecordQualityEvent(context.Context, QualityObservation, time.Duration) error
}

type PhysicalEventRecorder interface {
	RecordPhysicalEvents(context.Context, []attemptmeta.PhysicalFact) error
}
