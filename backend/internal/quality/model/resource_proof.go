package model

import (
	"fmt"
	"reflect"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// ClassifyResourceSample interprets complete measurements, never token counts.
func ClassifyResourceSample(s ResourceSample) string {
	if s.Conflict {
		return "conflict"
	}
	if !s.Completed || !s.UsageReported || !s.PathVerified || !s.IdentityVerified || s.UnexpectedOutput || s.PathKey == "" || s.Attempt.ID == "" || s.Sample != "token-short" || s.Attempt.Path.Status != attemptmeta.PathRegistered || s.Attempt.Path.Rotating || (s.PathFamily != 4 && s.PathFamily != 6) {
		return "unknown"
	}
	if s.Thinking && s.Outcome == MeasurementClean {
		return "A"
	}
	if !s.Thinking && s.PlainOutput && s.Outcome == MeasurementDegraded {
		return "B"
	}
	return "unknown"
}

type resourceEdge struct {
	account, exit string
	observation   ResourceObservation
}
type ResourceFacts struct {
	Normal   map[string]int
	Bad      map[string][]int
	Accounts map[uint64]string
	Nodes    map[uint64]string
	Conflict bool
}

// ResourceEvidence applies A => both normal, B+normal => other abnormal.
// All current observations are checked before extracting any certificate.
func ResourceEvidence(r ResourceCheckReport) ResourceFacts {
	f := ResourceFacts{Normal: map[string]int{}, Bad: map[string][]int{}, Accounts: map[uint64]string{}, Nodes: map[uint64]string{}}
	seen := map[string]ResourceObservation{}
	bindings := map[uint64]string{}
	generations := map[uint64]uint64{}
	identities := map[uint64]string{}
	edges := []resourceEdge{}
	for _, o := range r.Observations {
		if o.Window != r.Window {
			continue
		}
		class := ClassifyResourceSample(o.Sample)
		if o.Class == "conflict" || class == "conflict" {
			f.Conflict = true
		}
		s := o.Sample
		a := fmt.Sprintf("a:%d", o.IdentityGroup)
		e := fmt.Sprintf("e:%s:%d", s.PathKey, s.PathFamily)
		// Response validity and verified resource identity are independent facts.
		// An incomplete response cannot hide a changed anchor from this window.
		if s.Attempt.ID != "" && (s.Generated || class == "A" || class == "B") {
			if s.IdentityVerified && o.IdentityGroup != 0 && o.AccountID == s.Attempt.AccountID {
				if gen, ok := generations[o.AccountID]; ok && gen != s.CredentialGeneration {
					f.Conflict = true
				}
				if identity, ok := identities[o.AccountID]; ok && identity != a {
					f.Conflict = true
				}
				generations[o.AccountID], identities[o.AccountID] = s.CredentialGeneration, a
			}
			if s.PathVerified && s.PathKey != "" && o.NodeID == s.Attempt.Path.NodeID && s.Attempt.Path.Status == attemptmeta.PathRegistered && (s.PathFamily == 4 || s.PathFamily == 6) {
				binding := fmt.Sprintf("%s:%d:%d", e, s.PathBinding, s.Attempt.Path.Epoch)
				if old, ok := bindings[o.NodeID]; ok && old != binding {
					f.Conflict = true
				}
				bindings[o.NodeID] = binding
			}
		}
		if o.Class != "A" && o.Class != "B" {
			continue
		}
		if class != o.Class || o.ID < 1 || o.IdentityGroup == 0 || o.AccountID != o.Sample.Attempt.AccountID || o.NodeID != o.Sample.Attempt.Path.NodeID {
			f.Conflict = true
			continue
		}
		if old, ok := seen[s.Attempt.ID]; ok {
			if !reflect.DeepEqual(old.Sample, s) || old.AccountID != o.AccountID || old.NodeID != o.NodeID || old.IdentityGroup != o.IdentityGroup {
				f.Conflict = true
			}
			continue
		}
		seen[s.Attempt.ID] = o
		f.Accounts[o.AccountID], f.Nodes[o.NodeID] = a, e
		edges = append(edges, resourceEdge{a, e, o})
		if o.Class == "A" {
			if f.Normal[a] == 0 {
				f.Normal[a] = o.ID
			}
			if f.Normal[e] == 0 {
				f.Normal[e] = o.ID
			}
		}
	}
	for _, edge := range edges {
		if edge.observation.Class != "B" {
			continue
		}
		a, e, id := edge.account, edge.exit, edge.observation.ID
		if f.Normal[e] != 0 {
			f.Bad[a] = []int{id, f.Normal[e]}
		}
		if f.Normal[a] != 0 {
			f.Bad[e] = []int{id, f.Normal[a]}
		}
		if f.Normal[a] != 0 && f.Normal[e] != 0 {
			f.Conflict = true
		}
	}
	return f
}

func AssessResourceProofs(r ResourceCheckReport, now time.Time) ResourceCheckReport {
	f := ResourceEvidence(r)
	for i, p := range r.Results {
		// Closed windows retain history; only current-window proofs can change.
		if p.Window != 0 && p.Window != r.Window && p.Outcome != "inconclusive" {
			continue
		}
		p.Outcome, p.Reason, p.Rule, p.Evidence = "inconclusive", "insufficient_controls", "", []int{}
		p.Window, p.ValidUntil = r.Window, r.WindowStartedAt.Add(ResourceCheckWindow)
		key := f.Accounts[p.ResourceID]
		if p.Kind == "account" && p.IdentityGroup != 0 {
			key = fmt.Sprintf("a:%d", p.IdentityGroup)
		}
		if p.Kind == "node" {
			key = f.Nodes[p.ResourceID]
		}
		switch {
		case p.UnavailableReason != "":
			p.Reason = p.UnavailableReason
		case f.Conflict:
			p.Reason = "conflicting_samples"
		case !now.Before(p.ValidUntil):
			p.Reason = "window_expired"
		case f.Normal[key] != 0:
			p.Outcome, p.Reason, p.Rule, p.Evidence = "healthy", "proved_normal", "R1", []int{f.Normal[key]}
		case len(f.Bad[key]) != 0:
			p.Outcome, p.Reason, p.Rule, p.Evidence = "degraded", "proved_account_bad", "R2", f.Bad[key]
			if p.Kind == "node" {
				p.Reason, p.Rule = "proved_exit_bad", "R3"
			}
		}
		r.Results[i] = p
	}
	return r
}

// ResourceReportFor projects one selected resource without duplicating evidence.
func ResourceReportFor(r ResourceCheckReport, kind string, id uint64) ResourceCheckReport {
	r.Kind, r.ResourceID = kind, id
	for _, p := range r.Results {
		if p.Kind == kind && p.ResourceID == id {
			r.Outcome, r.Reason = p.Outcome, p.Reason
			return r
		}
	}
	return r
}
