package model

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// Event identity separates admission, completion and physical receipts.
type Event struct {
	Physical  *attemptmeta.PhysicalFact `json:"physical,omitempty"`
	Attempt   attemptmeta.Identity      `json:"attempt"`
	Stage     string                    `json:"stage"`
	Outcome   string                    `json:"outcome"`
	Rule      string                    `json:"rule,omitempty"`
	ErrorCode string                    `json:"error_code,omitempty"`
	At        time.Time                 `json:"at"`
	HoldUntil time.Time                 `json:"hold_until,omitempty"`
}

// 事件 stage/outcome 的持久化词汇是 events 用例、journal 校验与
// 存量行解码的三方共同合同:改字符串即破坏兼容,必须经此常量修改。
const (
	EventStageAdmission  = "admission"
	EventStageCompletion = "completion"
	EventStageExchange   = "exchange"
	EventStageRecovery   = "recovery"

	// EventOutcomeAdmitted 的持久值 "delivered" 表示"通过准入判决",
	// 不是完成声明(与 observation 的 OutcomeDelivered 含义不同)。
	EventOutcomeAdmitted    = "delivered"
	EventOutcomeDegraded    = "degraded"
	EventOutcomeRejected    = "rejected"
	EventOutcomeCompleted   = "completed"
	EventOutcomeInterrupted = "interrupted"
	EventOutcomeCanceled    = "canceled"
	EventOutcomeUnconfirmed = "unconfirmed"
	EventOutcomeObserved    = "observed"
)

type BacklogStats struct {
	InFlight        int64      `json:"in_flight"`
	Unconfirmed     int64      `json:"unconfirmed"`
	OldestExpiredAt *time.Time `json:"oldest_expired_at,omitempty"`
	Pending         int64      `json:"pending"`
	Limit           int64      `json:"limit"`
	OldestAt        *time.Time `json:"oldest_at,omitempty"`
	Leased          int64      `json:"leased"`
	Retrying        int64      `json:"retrying"`
}

func (e Event) ID() string { return e.Attempt.ID + "/" + e.Stage }
