package account

import (
	"errors"
	"math"
	"time"
)

const (
	FreeQuotaRecoveryPause = 24 * time.Hour
	PaidQuotaProbeRetry    = 15 * time.Minute
	QuotaProbeLease        = 5 * time.Minute
)

// QuotaRecoveryRef survives deletion of the recovery row. Generation identifies
// the actual upstream material; Revision identifies the state observed before IO.
type QuotaRecoveryRef struct {
	CredentialRef
	Revision uint64
}

func (c Credential) QuotaRecoveryRef() QuotaRecoveryRef {
	return QuotaRecoveryRef{CredentialRef: c.CredentialRef(), Revision: c.QuotaRecoveryRevision}
}

type RecoveryEventKind string

const (
	RecoveryFreeExhausted      RecoveryEventKind = "free_exhausted"
	RecoveryPaymentExhausted   RecoveryEventKind = "payment_exhausted"
	RecoveryProbeClaimed       RecoveryEventKind = "probe_claimed"
	RecoveryFreeProbeSucceeded RecoveryEventKind = "free_probe_succeeded"
	RecoveryPaidProbeFailed    RecoveryEventKind = "paid_probe_failed"
	RecoveryBillingObserved    RecoveryEventKind = "billing_observed"
)

type RecoveryEvent struct {
	Kind        RecoveryEventKind
	OccurredAt  time.Time
	Used, Limit int64
	Billing     *Billing
	AfterProbe  bool
}

type RecoveryResult struct {
	Ref           QuotaRecoveryRef
	ResetRevision uint64
	Recovery      *QuotaRecovery
	Applied       bool
	Recovered     bool
}

var (
	ErrQuotaRecoveryRevisionExhausted = errors.New("quota recovery revision exhausted")
	ErrQuotaRecoveryObservationStale  = errors.New("quota recovery observation is obsolete")
)

// TransitionQuotaRecovery is M07's Build recovery policy. Negative observations
// in one reset epoch merge conservatively; a successful probe or Billing result
// must still own the exact observed revision. Health/admin/risk are independent.
func TransitionQuotaRecovery(current Credential, recovery *QuotaRecovery, ref QuotaRecoveryRef, event RecoveryEvent, now time.Time) (RecoveryResult, error) {
	result := RecoveryResult{Ref: current.QuotaRecoveryRef(), ResetRevision: current.QuotaRecoveryResetRevision, Recovery: recovery}
	if current.Provider != ProviderBuild || current.CredentialRef() != ref.CredentialRef || ref.Revision > current.QuotaRecoveryRevision || ref.Revision < current.QuotaRecoveryResetRevision {
		return result, nil
	}
	negative := event.Kind == RecoveryFreeExhausted || event.Kind == RecoveryPaymentExhausted
	if !negative && ref.Revision != current.QuotaRecoveryRevision {
		return result, nil
	}
	now = now.UTC()
	var next *QuotaRecovery
	if recovery != nil {
		copy := *recovery
		next = &copy
	}
	clear := false
	switch event.Kind {
	case RecoveryFreeExhausted, RecoveryPaymentExhausted:
		due := now.Add(FreeQuotaRecoveryPause)
		kind := QuotaRecoveryKindFree
		if event.Kind == RecoveryPaymentExhausted && event.Billing != nil && event.Billing.IsPaid() {
			if end, ok := event.Billing.PeriodEnd(); ok && end.After(now) {
				due, kind = end, QuotaRecoveryKindPaid
			}
		}
		candidate := QuotaRecovery{AccountID: current.ID, Kind: kind, Status: QuotaRecoveryStatusExhausted, ConfirmedUsed: max(0, event.Used), ConfirmedLimit: max(0, event.Limit), ExhaustedAt: &now, LastConfirmedAt: &now, NextProbeAt: &due, UpdatedAt: now}
		if next != nil {
			candidate.ConfirmedUsed = max(candidate.ConfirmedUsed, next.ConfirmedUsed)
			candidate.ConfirmedLimit = max(candidate.ConfirmedLimit, next.ConfirmedLimit)
			if next.ExhaustedAt != nil && next.ExhaustedAt.Before(now) {
				candidate.ExhaustedAt = next.ExhaustedAt
			}
			if next.NextProbeAt != nil && next.NextProbeAt.After(due) {
				candidate.NextProbeAt = next.NextProbeAt
			}
			if next.Kind == QuotaRecoveryKindPaid {
				candidate.Kind = next.Kind
			}
		}
		next = &candidate
	case RecoveryProbeClaimed:
		if next == nil || (next.Status != QuotaRecoveryStatusExhausted && next.Status != QuotaRecoveryStatusProbing) || next.NextProbeAt == nil || now.Before(*next.NextProbeAt) {
			return result, nil
		}
		if !current.Enabled || current.AuthStatus != AuthStatusActive {
			return result, nil
		}
		due := now.Add(QuotaProbeLease)
		next.Status, next.NextProbeAt, next.UpdatedAt = QuotaRecoveryStatusProbing, &due, now
	case RecoveryFreeProbeSucceeded:
		if next == nil || next.Kind != QuotaRecoveryKindFree || next.Status != QuotaRecoveryStatusProbing {
			return result, nil
		}
		next, clear = nil, true
		result.Recovered = true
	case RecoveryPaidProbeFailed:
		if next == nil || next.Kind != QuotaRecoveryKindPaid || next.Status != QuotaRecoveryStatusProbing {
			return result, nil
		}
		due := now.Add(PaidQuotaProbeRetry)
		next.Status, next.NextProbeAt, next.UpdatedAt = QuotaRecoveryStatusExhausted, &due, now
	case RecoveryBillingObserved:
		if event.Billing == nil || event.Billing.AccountID != current.ID {
			return result, errors.New("billing observation account mismatch")
		}
		if event.AfterProbe && (next == nil || next.Kind != QuotaRecoveryKindPaid || next.Status != QuotaRecoveryStatusProbing) {
			return result, nil
		}
		billing := event.Billing
		result.Recovered = !billing.IsExhausted(current.MinimumRemaining)
		if !billing.IsPaid() || result.Recovered {
			if next != nil && next.Kind == QuotaRecoveryKindPaid {
				next, clear = nil, true
			}
		} else if end, ok := billing.PeriodEnd(); ok {
			due := end
			if !due.After(now) && event.AfterProbe {
				due = now.Add(PaidQuotaProbeRetry)
			}
			next = &QuotaRecovery{AccountID: current.ID, Kind: QuotaRecoveryKindPaid, Status: QuotaRecoveryStatusExhausted, ExhaustedAt: &now, NextProbeAt: &due, LastConfirmedAt: &now, UpdatedAt: now}
		} else if event.AfterProbe {
			due := now.Add(PaidQuotaProbeRetry)
			next.Status, next.NextProbeAt, next.UpdatedAt = QuotaRecoveryStatusExhausted, &due, now
		}
	default:
		return result, errors.New("invalid quota recovery event")
	}
	if current.QuotaRecoveryRevision >= math.MaxInt64 {
		return result, ErrQuotaRecoveryRevisionExhausted
	}
	result.Ref.Revision++
	if clear {
		result.ResetRevision = result.Ref.Revision
	}
	result.Recovery, result.Applied = next, true
	return result, nil
}
