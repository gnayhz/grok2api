package account

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const ModelAccessDeniedPause = 5 * time.Minute

type ModelRestrictionKind string

const (
	ModelQuotaExhausted ModelRestrictionKind = "model_quota_depleted"
	ModelAccessDenied   ModelRestrictionKind = "model_access_denied"
)

// ModelRestrictionEvent describes the response to an attempt made with Ref's
// material. Billing is that attempt's tier observation, never a billing write.
type ModelRestrictionEvent struct {
	Kind          ModelRestrictionKind
	UpstreamModel string
	RetryAfter    time.Duration
	Billing       *Billing
	OccurredAt    time.Time
}

type ModelRestrictionResult struct {
	Block   *ModelQuotaBlock
	Applied bool
}

// TransitionModelRestriction owns the independent per-model negative facts.
// Quota reset fences quota observations only; permission denials depend on the
// actual material generation. Neither event changes health or recovery clocks.
func TransitionModelRestriction(current Credential, existing *ModelQuotaBlock, ref QuotaRecoveryRef, event ModelRestrictionEvent, now time.Time) (ModelRestrictionResult, error) {
	result := ModelRestrictionResult{Block: existing}
	model := strings.TrimSpace(event.UpstreamModel)
	if model == "" || utf8.RuneCountInString(model) > 255 || (event.Kind != ModelQuotaExhausted && event.Kind != ModelAccessDenied) {
		return result, errors.New("invalid model restriction event")
	}
	if event.Billing != nil && event.Billing.AccountID != current.ID {
		return result, errors.New("model restriction billing account mismatch")
	}
	if current.CredentialRef() != ref.CredentialRef {
		return result, nil
	}
	if event.Kind == ModelQuotaExhausted && (ref.Revision < current.QuotaRecoveryResetRevision || ref.Revision > current.QuotaRecoveryRevision) {
		return result, nil
	}
	retry := event.RetryAfter
	if event.Kind == ModelQuotaExhausted {
		if (RoutingCandidate{Credential: current, Billing: event.Billing}).IsKnownFreeBuild() || retry <= 0 {
			retry = FreeQuotaRecoveryPause
		}
	} else if retry <= 0 {
		retry = ModelAccessDeniedPause
	}
	next := ModelQuotaBlock{AccountID: current.ID, UpstreamModel: model, Reason: string(event.Kind), CooldownUntil: now.UTC().Add(retry), UpdatedAt: now.UTC()}
	if existing != nil {
		if existing.AccountID != next.AccountID || existing.UpstreamModel != model || existing.Reason != next.Reason {
			return result, errors.New("model restriction state key mismatch")
		}
		if !next.CooldownUntil.After(existing.CooldownUntil) {
			return result, nil
		}
	}
	result.Block, result.Applied = &next, true
	return result, nil
}

// DominantModelRestriction is a deterministic routing projection. Facts remain
// independent in storage, including legacy reasons unknown to new writers.
func DominantModelRestriction(current, candidate ModelQuotaBlock) ModelQuotaBlock {
	if current.CooldownUntil.Before(candidate.CooldownUntil) ||
		(current.CooldownUntil.Equal(candidate.CooldownUntil) && candidate.Reason < current.Reason) {
		return candidate
	}
	return current
}
