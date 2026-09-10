package gateway

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"time"
)

// 底座缝隙接口(D3 三缝/B4 决议2):接口定义在底座侧,质量层实现,
// 组合根注入。未注入(nil)时底座按内建行为运行——可剥离性(D2 试金石)
// 由编译器与 nil 缺省共同保证。质量层概念(羁押/服刑/案件)不泄漏进
// 底座:底座只消费布尔与观测事实。

// AccountEligibility 是账号资格谓词缝隙(D3-1):底座选号时问
// "此人可调度吗"。判定规则(冻结/定罪)归质量层 Registry;
// 实现必须 O(1) 内存可判定、无锁(B2 性能约束)。
// All ordinary selection paths filter by this hint; after capacity and current
// credentials are acquired, they check it again unless AccountAdmission exists.
type AccountEligibility interface {
	AccountSchedulable(accountID uint64) bool
}

// AccountAdmission checks authoritative restrictions after acquiring capacity.
// A cached routing hint must never authorize a newer, committed restriction.
// This optional extension replaces the final AccountEligibility check.
type AccountAdmission interface {
	CheckAccountAdmission(context.Context, uint64) (bool, error)
}

// QualityObservationOutcome 是一次守卫判决的观测结果。
type QualityObservationOutcome string

const (
	// Admission satisfies the configured protocol rule only. The wire value
	// "delivered" is retained for journal compatibility, not a completion claim.
	QualityObservedAdmitted    QualityObservationOutcome = "delivered"
	QualityObservedDelivered                             = QualityObservedAdmitted
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

// QualityObserver 是流观察点缝隙(D3-3a):守卫判决的旁路出口。
// RecordQualityObservation 必须非阻塞(I19:满队丢弃计数,观测旁路
// 永不阻塞主路径)。
type QualityObserver interface {
	RecordQualityObservation(obs QualityObservation)
}

// QualityEventRecorder returns only after durable acceptance. Unlike the
// optional telemetry observer, this seam must not discard authoritative facts.
type QualityEventRecorder interface {
	RecordQualityEvent(context.Context, QualityObservation, time.Duration) error
}

type PhysicalEventRecorder interface {
	RecordPhysicalEvents(context.Context, []attemptmeta.PhysicalFact) error
}

// QualityRetryPolicy 是重试原语策略缝隙(D3-3b):扣留判决后的
// "再试一次"决策归质量层。nil=底座内建策略(decideQualityRetry,
// 现行行为,零变化)。质量层返回的动作仍经 boundQualityRetry 受
// 路由边界约束——底座保留"没有下一跳时不得空转重试"的兜底。
type QualityRetryPolicy interface {
	DecideQualityRetry(verdict QualityVerdict, attemptIndex, maxAttempts int, onExhausted string) QualityRetryAction
}

// QualityHoldKernel 是流判决内核缝隙(B4 决议2 依赖倒置:接口定义在
// 底座侧,质量层实现,组合根注入)。三规则判决语义(thinking→deliver /
// item_done→withhold / outrun→withhold / terminal→withhold / else wait)
// 归质量层 guard;底座只消费判决枚举,不 import 质量层任何包——
// "拔掉质量层底座仍编译"(D2 试金石/I13)由此保证。
// Production requires a kernel when the policy is enabled; missing dependencies
// fail explicitly. Standalone gateways use their immutable built-in kernel.
type QualityHoldKernel interface {
	ClassifyQualityHold(sig QualityStreamSignals) QualityVerdict
}
