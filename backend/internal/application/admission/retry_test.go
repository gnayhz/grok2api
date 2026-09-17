package admission

import (
	"testing"

	guardpolicy "github.com/chenyme/grok2api/backend/internal/domain/guard"
)

// The account-switch budget has exactly one rule. BudgetExhausted is the
// loop-facing form ("attemptsUsed account attempts are spent, no further
// account may be tried"); DecideRetry is the decision form for the attempt
// that just withheld. The boundary is maxAttempts attempts: the attempt at
// 0-based index maxAttempts-1 may still start, and its withhold ends the
// budget. Both forms must answer the same question for every count around
// that boundary.
func TestBudgetExhaustedMatchesDecideRetryAtBoundary(t *testing.T) {
	for _, maxAttempts := range []int{1, 2, 3, 6} {
		for _, used := range []int{maxAttempts - 1, maxAttempts, maxAttempts + 1} {
			wantExhausted := used >= maxAttempts
			if got := BudgetExhausted(used, maxAttempts); got != wantExhausted {
				t.Fatalf("BudgetExhausted(%d, %d) = %t, want %t", used, maxAttempts, got, wantExhausted)
			}
			if used < 1 {
				continue // DecideRetry starts at a completed 0-based attempt index
			}
			want := QualityActionRetry
			if wantExhausted {
				want = QualityActionReject
			}
			if got := DecideRetry(QualityWithhold, used-1, maxAttempts); got != want {
				t.Fatalf("DecideRetry(withhold, %d, %d) = %s, want %s: loop gate and decision rule disagree", used-1, maxAttempts, got, want)
			}
			// An exhausted budget must never soften into a delivery, even while
			// a routing candidate exists (fail-closed, G12).
			if commit := CommitHold(QualityWithhold, used-1, maxAttempts, true); commit.KeepBody || commit.Action == QualityActionDeliver {
				t.Fatalf("CommitHold(withhold, %d, %d, hasNext=true) = %+v, want a non-delivering action", used-1, maxAttempts, commit)
			}
		}
	}
}

// A zero or negative maxAttempts still bounds the loop: both forms fall back
// to the guard policy default instead of allowing unlimited account switches.
func TestBudgetExhaustedUsesGuardDefaultForUnconfiguredMaxAttempts(t *testing.T) {
	const defaultMax = guardpolicy.DefaultMaxAttempts
	for _, maxAttempts := range []int{0, -1} {
		if BudgetExhausted(defaultMax-1, maxAttempts) {
			t.Fatalf("maxAttempts=%d: attempt %d must still fit the budget", maxAttempts, defaultMax-1)
		}
		if !BudgetExhausted(defaultMax, maxAttempts) {
			t.Fatalf("maxAttempts=%d: budget must be spent after %d attempts", maxAttempts, defaultMax)
		}
		if got := DecideRetry(QualityWithhold, defaultMax-2, maxAttempts); got != QualityActionRetry {
			t.Fatalf("maxAttempts=%d: penultimate withhold must retry, got %s", maxAttempts, got)
		}
		if got := DecideRetry(QualityWithhold, defaultMax-1, maxAttempts); got != QualityActionReject {
			t.Fatalf("maxAttempts=%d: last withhold must reject, got %s", maxAttempts, got)
		}
	}
}
