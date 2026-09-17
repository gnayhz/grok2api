package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	admpkg "github.com/chenyme/grok2api/backend/internal/application/admission"
)

type (
	QualityRetryRuntime  = admpkg.QualityRetryRuntime
	QualityStreamSignals = admpkg.QualityStreamSignals
	QualityHoldKernel    = admpkg.QualityHoldKernel
	QualityJurisdiction  = admpkg.QualityJurisdiction
	QualityVerdict       = admpkg.QualityVerdict
	QualityRetryAction   = admpkg.QualityRetryAction
	QualityCommit        = admpkg.QualityCommit
	GuardSnapshot        = admpkg.GuardSnapshot
	GuardSnapshotSource  = admpkg.GuardSnapshotSource
)

const (
	QualityWait          = admpkg.QualityWait
	QualityDeliver       = admpkg.QualityDeliver
	QualityWithhold      = admpkg.QualityWithhold
	QualityActionDeliver = admpkg.QualityActionDeliver
	QualityActionRetry   = admpkg.QualityActionRetry
	QualityActionReject  = admpkg.QualityActionReject
)

var errAdmissionDeadline = admpkg.ErrDeadline

type admissionWait = admpkg.Wait

type admission struct {
	*admissionWait
}

func newAdmission(parent context.Context, started time.Time, budget time.Duration) *admission {
	return &admission{admissionWait: admpkg.NewWait(parent, started, budget)}
}

func (a *admission) setBudget(budget time.Duration) { a.SetBudget(budget) }
func (a *admission) disable()                       { a.Disable() }
func (a *admission) close()                         { a.Close() }

func (a *admission) commit() error  { return mapAdmissionError(a.Commit()) }
func (a *admission) failure() error { return mapAdmissionError(a.Failure()) }

func mapAdmissionError(err error) error {
	if errors.Is(err, admpkg.ErrDeadline) {
		return &UpstreamFailure{HTTPStatus: http.StatusGatewayTimeout, Code: "quality_admission_timeout",
			PublicMessage: "等待上游响应超过准入时限，请稍后重试", Cause: admpkg.ErrDeadline}
	}
	return err
}

func normalizeQualityRetry(cfg QualityRetryRuntime) QualityRetryRuntime {
	return admpkg.NormalizeRuntime(cfg)
}

func commitQualityHold(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool) QualityCommit {
	return admpkg.CommitHold(verdict, qualityAttempt, maxAttempts, hasNextRouting)
}

// qualityBudgetExhausted is the routing loop's view of the account-switch
// budget. The rule stays in admission so the loop gate and DecideRetry cannot
// drift apart.
func qualityBudgetExhausted(attemptsUsed, maxAttempts int) bool {
	return admpkg.BudgetExhausted(attemptsUsed, maxAttempts)
}
