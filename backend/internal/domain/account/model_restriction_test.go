package account

import (
	"strings"
	"testing"
	"time"
)

func TestModelRestrictionPolicyAndIdentity(t *testing.T) {
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	current := Credential{ID: 1, Provider: ProviderBuild, CredentialGeneration: 3, QuotaRecoveryRevision: 8, QuotaRecoveryResetRevision: 5, ObservedModel: "grok-4.5-build-free"}
	for _, test := range []struct {
		name       string
		kind       ModelRestrictionKind
		retry      time.Duration
		billing    *Billing
		revision   uint64
		generation uint64
		duration   time.Duration
		applied    bool
	}{
		{"free_fixed", ModelQuotaExhausted, time.Minute, nil, 8, 3, 24 * time.Hour, true},
		{"paid_hint", ModelQuotaExhausted, time.Hour, &Billing{AccountID: 1, PlanName: "SuperGrok"}, 8, 3, time.Hour, true},
		{"paid_default", ModelQuotaExhausted, 0, &Billing{AccountID: 1, PlanName: "SuperGrok"}, 8, 3, 24 * time.Hour, true},
		{"denied_default", ModelAccessDenied, 0, nil, 8, 3, 5 * time.Minute, true},
		{"denied_hint", ModelAccessDenied, time.Hour, nil, 8, 3, time.Hour, true},
		{"same_epoch_negative", ModelQuotaExhausted, 0, nil, 5, 3, 24 * time.Hour, true},
		{"old_epoch_quota", ModelQuotaExhausted, 0, nil, 4, 3, 0, false},
		{"future_revision_quota", ModelQuotaExhausted, 0, nil, 9, 3, 0, false},
		{"old_epoch_access_still_valid", ModelAccessDenied, 0, nil, 4, 3, 5 * time.Minute, true},
		{"old_material_quota", ModelQuotaExhausted, 0, nil, 8, 2, 0, false},
		{"old_material_access", ModelAccessDenied, 0, nil, 8, 2, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ref := current.QuotaRecoveryRef()
			ref.Revision = test.revision
			ref.Generation = test.generation
			event := ModelRestrictionEvent{Kind: test.kind, UpstreamModel: " model ", RetryAfter: test.retry, Billing: test.billing}
			got, err := TransitionModelRestriction(current, nil, ref, event, now)
			if err != nil || got.Applied != test.applied {
				t.Fatalf("applied=%t err=%v", got.Applied, err)
			}
			if test.applied && (got.Block.UpstreamModel != "model" || got.Block.Reason != string(test.kind) || !got.Block.CooldownUntil.Equal(now.Add(test.duration))) {
				t.Fatalf("wrong policy: %+v", got.Block)
			}
		})
	}
	for _, event := range []ModelRestrictionEvent{
		{Kind: "arbitrary_reason", UpstreamModel: "model"},
		{Kind: ModelAccessDenied, UpstreamModel: " "},
		{Kind: ModelQuotaExhausted, UpstreamModel: strings.Repeat("a", 256)},
		{Kind: ModelQuotaExhausted, UpstreamModel: "model", Billing: &Billing{AccountID: 2}},
	} {
		if _, err := TransitionModelRestriction(current, nil, current.QuotaRecoveryRef(), event, now); err == nil {
			t.Fatalf("invalid event accepted: %s", event.Kind)
		}
	}
}

func TestModelRestrictionMergesOnlySameReasonAndProjectsDeterministically(t *testing.T) {
	now := time.Now().UTC()
	current := Credential{ID: 1, Provider: ProviderBuild}
	existing := ModelQuotaBlock{AccountID: 1, UpstreamModel: "model", Reason: string(ModelAccessDenied), CooldownUntil: now.Add(time.Hour), UpdatedAt: now.Add(-time.Minute)}
	event := ModelRestrictionEvent{Kind: ModelAccessDenied, UpstreamModel: "model", RetryAfter: time.Minute}
	got, err := TransitionModelRestriction(current, &existing, current.QuotaRecoveryRef(), event, now)
	if err != nil || got.Applied || *got.Block != existing {
		t.Fatalf("short result replaced longer fact: %+v %v", got, err)
	}
	event.RetryAfter = 2 * time.Hour
	got, err = TransitionModelRestriction(current, &existing, current.QuotaRecoveryRef(), event, now)
	if err != nil || !got.Applied || !got.Block.CooldownUntil.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("extension failed: %+v %v", got, err)
	}
	event.Kind = ModelQuotaExhausted
	if _, err := TransitionModelRestriction(current, &existing, current.QuotaRecoveryRef(), event, now); err == nil {
		t.Fatal("different reason accepted as same state")
	}
	other := existing
	other.Reason = string(ModelQuotaExhausted)
	if DominantModelRestriction(existing, other) != existing || DominantModelRestriction(other, existing) != existing {
		t.Fatal("tied projection depends on SQL row order")
	}
	other.CooldownUntil = now.Add(2 * time.Hour)
	if DominantModelRestriction(existing, other) != other || DominantModelRestriction(other, existing) != other {
		t.Fatal("later restriction was hidden")
	}
}
