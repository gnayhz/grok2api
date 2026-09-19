package investigator

import (
	"fmt"
	"hash/fnv"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type resourcePair struct {
	account, node uint64
	purpose       string
}

// Rank only unmeasured edges that help remaining targets. Historical labels
// are absent: a normal endpoint must come from a current A observation.
func nextResourcePair(p model.ResourceCheckPlan, r model.ResourceCheckReport, group func(uint64) uint64) (resourcePair, bool) {
	f := model.ResourceEvidence(r)
	if f.Conflict {
		return resourcePair{}, false
	}
	accounts, nodes := append([]uint64{}, p.Accounts...), append([]uint64{}, p.Nodes...)
	remaining := []model.ResourceProof{}
	for _, t := range r.Results {
		if t.UnavailableReason != "" {
			continue
		}
		if t.Kind == "account" {
			accounts = append(accounts, t.ResourceID)
		} else {
			nodes = append(nodes, t.ResourceID)
		}
		if t.Outcome == "inconclusive" {
			remaining = append(remaining, t)
		}
	}
	if len(remaining) == 0 {
		return resourcePair{}, false
	}
	tried := map[string]bool{}
	blockedAccounts := map[uint64]bool{}
	negativeAccounts, negativeNodes := map[uint64]int{}, map[uint64]int{}
	for _, o := range r.Observations {
		if o.Window != r.Window {
			continue
		}
		tried[fmt.Sprintf("%d:%d", group(o.AccountID), o.NodeID)] = true
		if o.Class == "B" {
			negativeAccounts[group(o.AccountID)]++
			negativeNodes[o.NodeID]++
		}
		if o.Sample.Failure == model.ProbeFailureAccount || o.Sample.Failure == model.ProbeFailureCredential || o.Sample.Failure == model.ProbeFailureConfiguration || o.Sample.Failure == model.ProbeFailurePolicy || o.Sample.Failure == model.ProbeFailureHTTPRejected {
			blockedAccounts[group(o.AccountID)] = true
		}
	}
	eligible := remaining[:0]
	for _, t := range remaining {
		if t.Kind != "account" || !blockedAccounts[group(t.ResourceID)] {
			eligible = append(eligible, t)
		}
	}
	remaining = eligible
	if len(remaining) == 0 {
		return resourcePair{}, false
	}
	needsAnchor := false
	for _, t := range remaining {
		for _, o := range r.Observations {
			if o.Window == r.Window && o.Class == "B" && (t.Kind == "account" && t.ResourceID == o.AccountID || t.Kind == "node" && t.ResourceID == o.NodeID) {
				needsAnchor = true
			}
		}
	}
	best, score, tie := resourcePair{}, 0, uint64(0)
	for _, a := range accounts {
		for _, n := range nodes {
			if a == 0 || n == 0 || blockedAccounts[group(a)] || tried[fmt.Sprintf("%d:%d", group(a), n)] {
				continue
			}
			ak, nk := fmt.Sprintf("a:%d", group(a)), f.Nodes[n]
			if len(f.Bad[ak]) > 0 || len(f.Bad[nk]) > 0 {
				continue
			}
			alias := false
			for _, o := range r.Observations {
				if o.Window == r.Window && o.IdentityGroup == group(a) && nk != "" && f.Nodes[o.NodeID] == nk {
					alias = true
				}
			}
			if alias {
				continue
			}
			value, purpose := 0, "establish_control"
			if needsAnchor {
				value = 1
			}
			for _, t := range remaining {
				direct := t.Kind == "account" && t.ResourceID == a || t.Kind == "node" && t.ResourceID == n
				if direct {
					value += 100
					purpose = "measure_target"
					if t.Kind == "account" && f.Normal[nk] != 0 || t.Kind == "node" && f.Normal[ak] != 0 {
						value += 1000
					}
				}
				for _, o := range r.Observations {
					if o.Window != r.Window || o.Class != "B" {
						continue
					}
					if t.Kind == "account" && o.AccountID == t.ResourceID && o.NodeID == n || t.Kind == "node" && o.NodeID == t.ResourceID && o.AccountID == a {
						// After two negatives sharing an endpoint, cross the other
						// axis instead of spending the budget on the same ambiguity.
						bonus := 200
						if t.Kind == "account" && negativeNodes[n] >= 2 || t.Kind == "node" && negativeAccounts[group(a)] >= 2 {
							bonus = 20
						}
						value += bonus
						purpose = "resolve_negative"
					}
				}
			}
			h := fnv.New64a()
			fmt.Fprintf(h, "%d:%d:%d", p.Seed, a, n)
			rank := h.Sum64()
			if value > score || value == score && rank > tie {
				best, score, tie = resourcePair{a, n, purpose}, value, rank
			}
		}
	}
	return best, score > 0
}
