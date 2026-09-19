package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

func proofObservation(id int, a, n uint64, class string) ResourceObservation {
	s := ResourceSample{IdentityVerified: true, PathVerified: true, Completed: true, UsageReported: true, PlainOutput: true, PathKey: fmt.Sprintf("fictional-exit-%d", n), PathFamily: 4, Sample: "token-short", Outcome: MeasurementDegraded, Attempt: attemptmeta.Identity{ID: fmt.Sprintf("fictional-%d", id), AccountID: a, Path: attemptmeta.Path{NodeID: n, Status: attemptmeta.PathRegistered}}, InputTokens: 100}
	if class == "A" {
		s.Thinking = true
		s.Outcome = MeasurementClean
	}
	return ResourceObservation{ID: id, Window: 1, AccountID: a, NodeID: n, IdentityGroup: a, Class: class, Sample: s}
}

// The independent oracle enumerates every possible resource assignment.
func TestResourceProofExhaustiveThreeByThree(t *testing.T) {
	for pattern := 0; pattern < 19683; pattern++ {
		r := ResourceCheckReport{Window: 1}
		code := pattern
		for a := uint64(1); a <= 3; a++ {
			for n := uint64(1); n <= 3; n++ {
				v := code % 3
				code /= 3
				if v > 0 {
					class := "A"
					if v == 2 {
						class = "B"
					}
					r.Observations = append(r.Observations, proofObservation(len(r.Observations)+1, a, n, class))
				}
			}
		}
		possible := []int{}
		for world := 0; world < 64; world++ {
			fits := true
			for _, o := range r.Observations {
				normal := (world&(1<<(o.AccountID-1))) == 0 && (world&(1<<(o.NodeID+2))) == 0
				if (o.Class == "A") != normal {
					fits = false
					break
				}
			}
			if fits {
				possible = append(possible, world)
			}
		}
		f := ResourceEvidence(r)
		if f.Conflict != (len(possible) == 0) {
			t.Fatalf("conflict pattern=%d", pattern)
		}
		if f.Conflict {
			continue
		}
		for vertex := 0; vertex < 6; vertex++ {
			allNormal, allBad := true, true
			for _, w := range possible {
				if w&(1<<vertex) == 0 {
					allBad = false
				} else {
					allNormal = false
				}
			}
			key := f.Accounts[uint64(vertex+1)]
			if vertex >= 3 {
				key = f.Nodes[uint64(vertex-2)]
			}
			if (f.Normal[key] != 0) != allNormal || (len(f.Bad[key]) > 0) != allBad {
				t.Fatalf("pattern=%d vertex=%d", pattern, vertex)
			}
		}
	}
}
func TestResourceProofConflictAliasesWindowsAndAuxiliaryTokens(t *testing.T) {
	now := time.Now().UTC()
	r := ResourceCheckReport{Window: 1, WindowStartedAt: now, Results: []ResourceProof{{ResourceTarget: ResourceTarget{Kind: "account", ResourceID: 1}}}, Observations: []ResourceObservation{proofObservation(1, 1, 8, "B"), proofObservation(2, 2, 8, "A")}}
	r = AssessResourceProofs(r, now)
	if r.Results[0].Outcome != "degraded" || len(r.Results[0].Evidence) != 2 {
		t.Fatal(r.Results)
	}
	r.Observations = append(r.Observations, proofObservation(3, 1, 9, "A"))
	r = AssessResourceProofs(r, now)
	if r.Results[0].Reason != "conflicting_samples" {
		t.Fatal("discarded counterevidence", r.Results)
	}
	r.Window = 2
	r.WindowStartedAt = now
	r = AssessResourceProofs(r, now)
	if r.Results[0].Outcome != "inconclusive" {
		t.Fatal("reused old normal evidence")
	}
	for _, class := range []string{"A", "B"} {
		s := proofObservation(1, 1, 8, class).Sample
		for _, tokens := range []int64{0, 100, 999} {
			s.InputTokens = tokens
			if ClassifyResourceSample(s) != class {
				t.Fatal("token count used as identity")
			}
		}
		s.UsageReported = false
		if ClassifyResourceSample(s) != "unknown" {
			t.Fatal("missing usage accepted")
		}
	}
	alias := proofObservation(3, 1, 9, "A")
	alias.Sample.PathKey = r.Observations[0].Sample.PathKey
	r.Window = 1
	r.Observations = append(r.Observations[:2], alias)
	if !ResourceEvidence(r).Conflict {
		t.Fatal("same exit hidden behind node alias")
	}
	duplicate := r.Observations[0]
	duplicate.ID = 4
	duplicate.Sample.Thinking = true
	duplicate.Class = "A"
	duplicate.Sample.Outcome = MeasurementClean
	r.Observations = append(r.Observations[:2], duplicate)
	if !ResourceEvidence(r).Conflict {
		t.Fatal("same physical attempt overwritten")
	}
}

func TestResourceProofRejectsIncompleteEvidenceAndIdentityDrift(t *testing.T) {
	for _, mutate := range []func(*ResourceSample){
		func(s *ResourceSample) { s.Completed = false },
		func(s *ResourceSample) { s.IdentityVerified = false },
		func(s *ResourceSample) { s.PathVerified = false },
		func(s *ResourceSample) { s.PathFamily = 0 },
		func(s *ResourceSample) { s.Attempt.Path.Status = attemptmeta.PathUnknown },
		func(s *ResourceSample) { s.PlainOutput = false },
	} {
		o := proofObservation(1, 1, 1, "B")
		mutate(&o.Sample)
		if ClassifyResourceSample(o.Sample) != "unknown" {
			t.Fatal("incomplete evidence classified as B")
		}
		if !ResourceEvidence(ResourceCheckReport{Window: 1, Observations: []ResourceObservation{o}}).Conflict {
			t.Fatal("trusted invalid caller classification")
		}
	}
	for _, mutate := range []func(*ResourceObservation){
		func(o *ResourceObservation) { o.Sample.CredentialGeneration++ },
		func(o *ResourceObservation) { o.Sample.PathBinding++ },
		func(o *ResourceObservation) { o.IdentityGroup++ },
	} {
		a, b := proofObservation(1, 1, 1, "A"), proofObservation(2, 1, 1, "A")
		mutate(&b)
		if !ResourceEvidence(ResourceCheckReport{Window: 1, Observations: []ResourceObservation{a, b}}).Conflict {
			t.Fatal("merged changing identity/path")
		}
	}
}

func TestResourceProofSharedIdentityAndClosedWindow(t *testing.T) {
	now := time.Now().UTC()
	r := ResourceCheckReport{Window: 1, WindowStartedAt: now, Observations: []ResourceObservation{proofObservation(1, 1, 2, "A")}, Results: []ResourceProof{
		{ResourceTarget: ResourceTarget{Kind: "account", ResourceID: 1}, IdentityGroup: 1},
		{ResourceTarget: ResourceTarget{Kind: "account", ResourceID: 3}, IdentityGroup: 1},
		{ResourceTarget: ResourceTarget{Kind: "account", ResourceID: 4}, IdentityGroup: 4},
	}}
	r = AssessResourceProofs(r, now)
	if r.Results[0].Outcome != "healthy" || r.Results[1].Outcome != "healthy" || r.Results[1].Evidence[0] != 1 {
		t.Fatal("known aliases repeated the physical measurement", r.Results)
	}
	r.Window, r.WindowStartedAt = 2, now.Add(ResourceCheckWindow)
	r.Observations = append(r.Observations, proofObservation(2, 4, 2, "B"))
	r.Observations[1].Window = 2
	r = AssessResourceProofs(r, r.WindowStartedAt)
	if r.Results[0].Outcome != "healthy" || r.Results[0].Window != 1 || r.Results[2].Outcome != "inconclusive" {
		t.Fatal("historical certificate lost or stale anchor reused", r.Results)
	}
}

func TestResourceProofUnknownResponseCannotHideVerifiedPathDrift(t *testing.T) {
	for _, change := range []string{"address", "binding", "epoch", "identity"} {
		t.Run(change, func(t *testing.T) {
			normal := proofObservation(1, 1, 8, "A")
			failed := proofObservation(2, 1, 8, "A")
			failed.Class = "unknown"
			failed.Sample.Generated = true
			failed.Sample.Completed = false
			failed.Sample.Outcome = MeasurementError
			switch change {
			case "address":
				failed.Sample.PathKey = "fictional-changed-exit"
			case "binding":
				failed.Sample.PathBinding++
			case "epoch":
				failed.Sample.Attempt.Path.Epoch++
			case "identity":
				failed.IdentityGroup++
			}
			negative := proofObservation(3, 2, 9, "B")
			negative.Sample.PathKey = normal.Sample.PathKey
			if !ResourceEvidence(ResourceCheckReport{Window: 1, Observations: []ResourceObservation{normal, failed, negative}}).Conflict {
				t.Fatal("verified resource change disappeared because the response was incomplete")
			}
		})
	}
}
