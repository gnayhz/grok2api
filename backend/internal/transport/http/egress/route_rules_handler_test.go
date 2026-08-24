package egress

import (
	"testing"

	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// Regression: a PUT body without scope/class targets must still parse; nil
// maps mean "keep the stored targets", so they must round-trip as nil.
func TestOperationsConfigRequestInputKeepsDefaultTargetWithoutMaps(t *testing.T) {
	request := operationsConfigRequest{
		ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900,
		DefaultTarget: &routingTargetRequest{Mode: "node", NodeID: "21"},
	}
	input, err := request.input()
	if err != nil {
		t.Fatal(err)
	}
	if input.DefaultTarget == nil || input.DefaultTarget.NodeID != 21 {
		t.Fatalf("default target = %#v", input.DefaultTarget)
	}
	if input.ScopeTargets != nil || input.ClassTargets != nil {
		t.Fatalf("scope/class targets = %#v/%#v, want nil (keep-stored semantics)", input.ScopeTargets, input.ClassTargets)
	}
}

func TestOperationsConfigRequestInputNilMapsStayNil(t *testing.T) {
	request := operationsConfigRequest{ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900}
	input, err := request.input()
	if err != nil {
		t.Fatal(err)
	}
	if input.DefaultTarget != nil {
		t.Fatalf("default target = %#v, want nil", input.DefaultTarget)
	}
	if input.ScopeTargets != nil || input.ClassTargets != nil {
		t.Fatalf("scope/class targets = %#v/%#v, want nil", input.ScopeTargets, input.ClassTargets)
	}
}

func TestOperationsConfigRequestInputParsesAllTargetLevels(t *testing.T) {
	request := operationsConfigRequest{
		ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900,
		ScopeTargets: map[string]routingTargetRequest{"grok_build": {Mode: "direct"}},
		ClassTargets: map[string]routingTargetRequest{"billing": {Mode: "node", NodeID: "21"}},
	}
	input, err := request.input()
	if err != nil {
		t.Fatal(err)
	}
	if target := input.ScopeTargets[egressdomain.ScopeBuild]; target.Mode != egressdomain.RoutingTargetDirect {
		t.Fatalf("build scope target = %#v", target)
	}
	if target := input.ClassTargets[egressdomain.TrafficClassBilling]; target.Mode != egressdomain.RoutingTargetNode || target.NodeID != 21 {
		t.Fatalf("billing class target = %#v", target)
	}
}

func TestOperationsConfigRequestInputRejectsZeroTargetNodeID(t *testing.T) {
	request := operationsConfigRequest{
		ClassTargets: map[string]routingTargetRequest{"billing": {Mode: "node", NodeID: "0"}},
	}
	if _, err := request.input(); err == nil {
		t.Fatal("expected error for zero target node id")
	}
}
