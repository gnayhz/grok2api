package admission

import guardpolicy "github.com/chenyme/grok2api/backend/internal/domain/guard"

type QualityVerdict string

const (
	QualityWait     QualityVerdict = "wait"
	QualityDeliver  QualityVerdict = "deliver"
	QualityWithhold QualityVerdict = "withhold"
)

type QualityRetryAction string

const (
	QualityActionDeliver QualityRetryAction = "deliver"
	QualityActionRetry   QualityRetryAction = "retry"
	QualityActionReject  QualityRetryAction = "reject"
)

type QualityCommit struct {
	Action   QualityRetryAction
	Audit    bool
	KeepBody bool
}

// BudgetExhausted reports whether attemptsUsed account attempts have consumed
// the whole account-switch retry budget, so the routing loop must not start
// another account attempt. It is the loop-facing form of the rule DecideRetry
// applies to a completed attempt: a withhold at 0-based index i may switch
// accounts exactly while BudgetExhausted(i+1, maxAttempts) is false.
func BudgetExhausted(attemptsUsed, maxAttempts int) bool {
	if maxAttempts <= 0 {
		maxAttempts = guardpolicy.DefaultMaxAttempts
	}
	if attemptsUsed < 0 {
		attemptsUsed = 0
	}
	return attemptsUsed >= maxAttempts
}

func DecideRetry(verdict QualityVerdict, attemptIndex, maxAttempts int) QualityRetryAction {
	if verdict == QualityDeliver {
		return QualityActionDeliver
	}
	if verdict != QualityWithhold {
		return QualityActionReject
	}
	if attemptIndex < 0 {
		attemptIndex = 0
	}
	// attemptIndex is 0-based: once it withholds, attemptIndex+1 attempts are
	// spent, which is what the shared budget predicate measures.
	if !BudgetExhausted(attemptIndex+1, maxAttempts) {
		return QualityActionRetry
	}
	return QualityActionReject
}

func BoundRetry(action QualityRetryAction, hasNextRoutingAttempt bool) QualityRetryAction {
	if action != QualityActionRetry || hasNextRoutingAttempt {
		return action
	}
	return QualityActionReject
}

func CommitHold(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool) QualityCommit {
	action := BoundRetry(DecideRetry(verdict, qualityAttempt, maxAttempts), hasNextRouting)
	switch action {
	case QualityActionRetry, QualityActionReject:
		return QualityCommit{Action: action, Audit: true, KeepBody: false}
	default:
		return QualityCommit{Action: QualityActionDeliver, Audit: false, KeepBody: true}
	}
}
