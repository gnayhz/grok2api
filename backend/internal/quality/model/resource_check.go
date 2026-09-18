package model

import "time"

const ResourceCheckVersion = "resource-quality-check-v1"
const ProbeResourceCheck ProbeDirection = "resource_check"
const ResourceCheckMaxGroups = 4
const ResourceCheckMaxCalls = 28
const ResourceCheckTimeout = 12 * time.Minute
const ResourceCheckQueueTimeout = 4 * time.Hour

// A plan freezes candidates, not their health. Every control must establish a
// fresh positive observation during execution; fleet membership is not evidence.
type ResourceCheckPlan struct {
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
	Kind              string   `json:"kind"`
	ResourceID        uint64   `json:"resource_id"`
	Accounts          []uint64 `json:"accounts"`
	Nodes             []uint64 `json:"nodes"`
}

type ResourceCheckGroup struct {
	ControlAccount uint64               `json:"control_account"`
	ControlNode    uint64               `json:"control_node"`
	AccountID      uint64               `json:"account_id"`
	NodeID         uint64               `json:"node_id"`
	IdentityGroup  uint64               `json:"identity_group"`
	Control        []AccountCheckSample `json:"control"`
	Samples        []AccountCheckSample `json:"samples"`
	After          *AccountCheckSample  `json:"after,omitempty"`
	Outcome        string               `json:"outcome"`
	Reason         string               `json:"reason"`
	ControlDelta   int64                `json:"control_delta"`
	Delta          int64                `json:"delta"`
}

type ResourceCheckReport struct {
	Version    string               `json:"version"`
	Kind       string               `json:"kind"`
	ResourceID uint64               `json:"resource_id"`
	Outcome    string               `json:"outcome"`
	Reason     string               `json:"reason"`
	Calls      int                  `json:"calls"`
	MaxCalls   int                  `json:"max_calls"`
	Groups     []ResourceCheckGroup `json:"groups"`
}

type ResourceCheck struct {
	ID         uint64               `json:"id"`
	Kind       string               `json:"kind"`
	ResourceID uint64               `json:"resource_id"`
	Model      string               `json:"model"`
	State      ProbeTaskState       `json:"state"`
	CreatedAt  time.Time            `json:"created_at"`
	FinishedAt *time.Time           `json:"finished_at,omitempty"`
	Report     *ResourceCheckReport `json:"report,omitempty"`
}

func IsManualProbe(direction ProbeDirection) bool {
	return direction == ProbeAccountCheck || direction == ProbeResourceCheck
}

// ResourceFingerprint accepts only completed, stable, explicitly pinned
// measurements. Neither an HTTP failure nor a token claim can establish B.
func ResourceFingerprint(samples []AccountCheckSample) (delta int64, class string, valid bool) {
	if len(samples) != 3 || samples[0].Sample != "token-short" || samples[1].Sample != "token-long" || samples[2].Sample != "token-short" {
		return
	}
	base := samples[0]
	if base.PathKey == "" || base.InputTokens != samples[2].InputTokens {
		return
	}
	thinking, missing := 0, 0
	seen := map[string]bool{}
	for _, s := range samples {
		if !s.Completed || !s.UsageReported || s.InputTokens <= 0 || !s.PathVerified || s.PathKey != base.PathKey || s.PathBinding != base.PathBinding || s.Attempt.ID == "" || seen[s.Attempt.ID] || s.Attempt.AccountID != base.Attempt.AccountID || s.Attempt.Path.NodeID != base.Attempt.Path.NodeID || s.Attempt.Path.Epoch != base.Attempt.Path.Epoch {
			return 0, "", false
		}
		seen[s.Attempt.ID] = true
		if s.Thinking && s.Outcome == MeasurementClean {
			thinking++
		}
		if !s.Thinking && s.Outcome == MeasurementDegraded {
			missing++
		}
	}
	delta = samples[1].InputTokens - base.InputTokens
	if delta <= 0 {
		return 0, "", false
	}
	if thinking == 3 {
		return delta, "A", true
	}
	if missing == 3 {
		return delta, "B", true
	}
	return 0, "", false
}

func AssessResourceGroup(kind string, g ResourceCheckGroup) ResourceCheckGroup {
	g.Outcome, g.Reason = "inconclusive", "control_unavailable"
	controlDelta, controlClass, ok := ResourceFingerprint(g.Control)
	if !ok || controlClass != "A" {
		return g
	}
	g.ControlDelta = controlDelta
	g.Reason = "measurement_unavailable"
	delta, class, ok := ResourceFingerprint(g.Samples)
	if !ok {
		return g
	}
	g.Delta = delta
	g.Reason = "control_changed"
	after, before := g.After, g.Control[0]
	if after == nil || !after.Completed || !after.UsageReported || !after.PathVerified || !after.Thinking || after.Outcome != MeasurementClean || after.Sample != "token-short" || after.InputTokens != before.InputTokens || after.PathKey != before.PathKey || after.PathBinding != before.PathBinding || after.Attempt.AccountID != before.Attempt.AccountID || after.Attempt.Path.NodeID != before.Attempt.Path.NodeID || after.Attempt.Path.Epoch != before.Attempt.Path.Epoch {
		return g
	}
	g.Reason = "path_unverified"
	if kind == "account" && (g.ControlNode != g.NodeID || before.PathKey != g.Samples[0].PathKey || before.PathBinding != g.Samples[0].PathBinding) {
		return g
	}
	if kind == "node" && (g.ControlAccount != g.AccountID || g.ControlNode == g.NodeID || before.PathKey == g.Samples[0].PathKey) {
		return g
	}
	if class == "A" && delta == controlDelta {
		g.Outcome, g.Reason = "healthy", "thinking_and_tokens_match"
		return g
	}
	if class == "B" && delta != controlDelta {
		g.Outcome, g.Reason = "degraded", "thinking_and_tokens_differ"
		return g
	}
	g.Reason = "signals_conflict"
	return g
}

// Two positive comparisons establish current usability; a negative resource
// attribution needs three independent comparisons. A contrary measured class
// is retained even when its control is incomplete, never voted away.
func AssessResourceCheck(report ResourceCheckReport) ResourceCheckReport {
	if report.Reason == "path_unverified" && len(report.Groups) == 0 {
		report.Outcome = "inconclusive"
		return report
	}
	report.Outcome, report.Reason = "inconclusive", "insufficient_controls"
	healthy, degraded := 0, 0
	seen := map[string]bool{}
	identities := map[uint64]bool{}
	sawA, sawB := false, false
	conflictingSignals := false
	var calibration int64
	var targetPath string
	var targetBinding uint64
	controlIdentities := map[uint64]bool{}
	for _, g := range report.Groups {
		if g.Reason == "signals_conflict" {
			conflictingSignals = true
		}
		for _, s := range g.Samples {
			if s.Completed && s.Thinking {
				sawA = true
			}
			if s.Completed && s.Outcome == MeasurementDegraded && !s.Thinking {
				sawB = true
			}
		}
		if g.Outcome != "healthy" && g.Outcome != "degraded" {
			continue
		}
		if report.Kind == "node" {
			if targetPath != "" && (targetPath != g.Samples[0].PathKey || targetBinding != g.Samples[0].PathBinding) {
				report.Reason = "path_changed"
				return report
			}
			targetPath, targetBinding = g.Samples[0].PathKey, g.Samples[0].PathBinding
		}
		if calibration != 0 && calibration != g.ControlDelta {
			report.Reason = "calibration_changed"
			return report
		}
		calibration = g.ControlDelta
		controlIdentities[g.IdentityGroup] = true
		if report.Kind == "account" {
			key := g.Samples[0].PathKey
			if seen[key] {
				continue
			}
			seen[key] = true
		} else {
			if g.IdentityGroup == 0 || identities[g.IdentityGroup] {
				continue
			}
			identities[g.IdentityGroup] = true
		}
		if g.Outcome == "healthy" {
			healthy++
		} else {
			degraded++
		}
	}
	if conflictingSignals || sawA && sawB {
		report.Reason = "conflicting_samples"
		return report
	}
	if healthy >= 2 && !sawB {
		report.Outcome, report.Reason = "healthy", "controlled_checks_passed"
	}
	if degraded >= 3 && !sawA && len(controlIdentities) >= 2 {
		report.Outcome, report.Reason = "degraded", "resource_attribution_confirmed"
	}
	return report
}
