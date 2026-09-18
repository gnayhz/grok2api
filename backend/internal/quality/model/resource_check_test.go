package model

import (
	"fmt"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

func syntheticResourceSamples(account, node uint64, path string, thinking bool, delta int64) []AccountCheckSample {
	items := []AccountCheckSample{}
	for i, name := range []string{"token-short", "token-long", "token-short"} {
		input := int64(100)
		if i == 1 {
			input += delta
		}
		outcome := MeasurementClean
		if !thinking {
			outcome = MeasurementDegraded
		}
		items = append(items, AccountCheckSample{Sample: name, Attempt: attemptmeta.Identity{ID: fmt.Sprintf("fictional-%d-%d-%d", account, node, i), AccountID: account, Path: attemptmeta.Path{NodeID: node, Epoch: 1}}, PathKey: path, PathVerified: true, PathBinding: 1, Thinking: thinking, Completed: true, UsageReported: true, InputTokens: input, Outcome: outcome})
	}
	return items
}
func syntheticResourceGroup(kind string, index int, thinking bool) ResourceCheckGroup {
	account, node := uint64(100+index), uint64(10+index)
	g := ResourceCheckGroup{ControlAccount: account, ControlNode: node, IdentityGroup: account, AccountID: account, NodeID: node}
	path := fmt.Sprintf("fictional-path-%d", node)
	g.Control = syntheticResourceSamples(account, node, path, true, 180)
	if kind == "account" {
		g.AccountID = 7
	} else {
		g.NodeID = 9
		path = "fictional-target"
	}
	delta := int64(180)
	if !thinking {
		delta = 90
	}
	g.Samples = syntheticResourceSamples(g.AccountID, g.NodeID, path, thinking, delta)
	after := g.Control[0]
	after.Attempt.ID += "-after"
	g.After = &after
	return AssessResourceGroup(kind, g)
}
func TestResourceCheckAttributionRequiresIndependentControls(t *testing.T) {
	for _, kind := range []string{"account", "node"} {
		for _, tc := range []struct {
			name     string
			thinking bool
			count    int
			want     string
		}{{"two healthy", true, 2, "healthy"}, {"two bad insufficient", false, 2, "inconclusive"}, {"three bad", false, 3, "degraded"}} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				r := ResourceCheckReport{Kind: kind}
				for i := 0; i < tc.count; i++ {
					r.Groups = append(r.Groups, syntheticResourceGroup(kind, i, tc.thinking))
				}
				if got := AssessResourceCheck(r); got.Outcome != tc.want {
					t.Fatalf("%+v", got)
				}
			})
		}
	}
}
func TestResourceCheckRejectsConflictsAndUnverifiedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ResourceCheckGroup)
	}{
		{"incomplete", func(g *ResourceCheckGroup) { g.Samples[1].Completed = false }},
		{"usage absent", func(g *ResourceCheckGroup) { g.Samples[1].UsageReported = false }},
		{"changed short", func(g *ResourceCheckGroup) { g.Samples[2].InputTokens++ }},
		{"rotating path", func(g *ResourceCheckGroup) { g.Samples[1].PathVerified = false }},
		{"changed binding", func(g *ResourceCheckGroup) { g.Samples[1].PathBinding++ }},
		{"duplicate attempt", func(g *ResourceCheckGroup) { g.Samples[1].Attempt.ID = g.Samples[0].Attempt.ID }},
		{"claimed tokens match", func(g *ResourceCheckGroup) { g.Samples[1].InputTokens = 280 }},
		{"control changed", func(g *ResourceCheckGroup) { g.After.Thinking = false }},
		{"control other path", func(g *ResourceCheckGroup) {
			for i := range g.Control {
				g.Control[i].PathKey = "different"
			}
			g.After.PathKey = "different"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := syntheticResourceGroup("account", 0, false)
			tc.edit(&g)
			if got := AssessResourceGroup("account", g); got.Outcome != "inconclusive" {
				t.Fatalf("%+v", got)
			}
		})
	}
	r := ResourceCheckReport{Kind: "account", Groups: []ResourceCheckGroup{syntheticResourceGroup("account", 0, false), syntheticResourceGroup("account", 1, false), syntheticResourceGroup("account", 2, false)}}
	other := syntheticResourceGroup("account", 3, true)
	other.Outcome = "inconclusive"
	r.Groups = append(r.Groups, other)
	if got := AssessResourceCheck(r); got.Reason != "conflicting_samples" {
		t.Fatalf("counterevidence lost: %+v", got)
	}
	r = ResourceCheckReport{Kind: "account", Groups: []ResourceCheckGroup{syntheticResourceGroup("account", 0, false), syntheticResourceGroup("account", 0, false), syntheticResourceGroup("account", 0, false)}}
	if got := AssessResourceCheck(r); got.Outcome != "inconclusive" {
		t.Fatal("duplicate IP counted")
	}
	r = ResourceCheckReport{Kind: "node", Groups: []ResourceCheckGroup{syntheticResourceGroup("node", 0, false), syntheticResourceGroup("node", 1, false), syntheticResourceGroup("node", 2, false)}}
	r.Groups[2].Samples[0].PathKey = "another-exit"
	if got := AssessResourceCheck(r); got.Reason != "path_changed" {
		t.Fatal("changed target aggregated")
	}
}
