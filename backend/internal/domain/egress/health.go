package egress

import "time"

type HealthObservationKind uint8

const (
	HealthSuccess HealthObservationKind = iota + 1
	HealthTransportFailure
	HealthAntiBotRejection
)

// HealthObservation describes a completed physical request against the binding
// captured when its lease was issued. Success is conditional on that health
// revision; it cannot erase a failure/quarantine established after acquisition.
// Consecutive failures can be combined without losing their count.
type HealthObservation struct {
	NodeID            uint64
	EncryptedProxyURL string
	BindingRevision   uint64
	ExpectedRevision  uint64
	Kind              HealthObservationKind
	Failures          int
	CooldownUntil     *time.Time // Minimum transport cooldown, e.g. confirmed dead exit.
	ObservedAt        time.Time
}

// HealthState is the runtime projection shared by the observation processor
// and storage. It intentionally carries no administrator display associations.
type HealthState struct {
	Revision      uint64
	Health        float64
	FailureCount  int
	CooldownUntil *time.Time
	LastError     string
}

func (n Node) HealthState() HealthState {
	return HealthState{Revision: n.HealthRevision, Health: n.Health, FailureCount: n.FailureCount, CooldownUntil: n.CooldownUntil, LastError: n.LastError}
}

func (s HealthState) ApplyTo(n Node) Node {
	n.HealthRevision, n.Health, n.FailureCount, n.CooldownUntil, n.LastError = s.Revision, s.Health, s.FailureCount, s.CooldownUntil, s.LastError
	return n
}

// Apply uses the same transition as storage. Revision checks reject an old
// success locally before it can affect routing; storage repeats the check for
// other replicas and configuration changes.
func (s HealthState) Apply(o HealthObservation) (HealthState, bool) {
	if o.Kind == HealthSuccess {
		if s.Revision != o.ExpectedRevision {
			return s, false
		}
		s.Health = min(1, s.Health+0.1)
		s.FailureCount = 0
		if s.LastError != LastErrorExitIPQuality {
			s.CooldownUntil, s.LastError = nil, ""
		}
		s.Revision++
		return s, true
	}
	count := max(1, o.Failures)
	for i := 0; i < min(count, 32); i++ {
		s.Health = max(0.05, s.Health*0.7)
	}
	s.FailureCount += count
	s.Revision += uint64(count)
	if s.LastError == LastErrorExitIPQuality {
		return s, true
	}
	if o.Kind == HealthAntiBotRejection {
		// Rejection is not evidence of transport recovery. Preserve a prior
		// cooldown (and its reason), even when this request predates it.
		if s.CooldownUntil == nil {
			s.LastError = "anti-bot rejection"
		}
		return s, true
	}
	until := o.ObservedAt.Add(30 * time.Second * time.Duration(1<<min(s.FailureCount-1, 4)))
	if o.CooldownUntil != nil && o.CooldownUntil.After(until) {
		until = *o.CooldownUntil
	}
	if s.CooldownUntil != nil && s.CooldownUntil.After(until) {
		until = *s.CooldownUntil
	}
	s.CooldownUntil, s.LastError = &until, LastErrorTransport
	return s, true
}

// MergeFailures combines observations for the same binding. A transport
// failure must survive a later anti-bot rejection in a coalesced queue; only
// transport timestamps contribute to backoff, and explicit cooldowns are floors.
// Both observations must describe failures, not recovery.
func (o HealthObservation) MergeFailures(previous HealthObservation) HealthObservation {
	o.Failures = max(1, o.Failures) + max(1, previous.Failures)
	if previous.Kind != HealthTransportFailure {
		return o
	}
	if o.Kind != HealthTransportFailure {
		o.Kind, o.ObservedAt, o.CooldownUntil = previous.Kind, previous.ObservedAt, previous.CooldownUntil
		return o
	}
	if previous.ObservedAt.After(o.ObservedAt) {
		o.ObservedAt = previous.ObservedAt
	}
	if previous.CooldownUntil != nil && (o.CooldownUntil == nil || previous.CooldownUntil.After(*o.CooldownUntil)) {
		o.CooldownUntil = previous.CooldownUntil
	}
	return o
}
