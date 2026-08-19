package egress

import (
	"testing"

	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// Regression: a PUT body without the fallbacks field must still carry
// routeRules into the service input. The historical early return on nil
// fallbacks silently dropped the rules payload.
func TestOperationsConfigRequestInputKeepsRouteRulesWithoutFallbacks(t *testing.T) {
	request := operationsConfigRequest{
		ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []operationsRouteRuleRequest{
			{Scope: "grok_build", Class: "billing", TargetMode: "direct", Enabled: true},
			{Scope: "grok_build", Class: "credential", TargetMode: "fixed", TargetNodeID: "21", Enabled: true},
		},
	}
	input, err := request.input()
	if err != nil {
		t.Fatal(err)
	}
	if input.RouteRules == nil {
		t.Fatal("route rules were dropped when fallbacks were omitted")
	}
	if len(input.RouteRules) != 2 {
		t.Fatalf("route rules = %#v, want 2 entries", input.RouteRules)
	}
	if input.RouteRules[0].Class != egressdomain.TrafficClassBilling || input.RouteRules[0].TargetMode != egressdomain.RouteRuleTargetDirect {
		t.Fatalf("first rule = %#v", input.RouteRules[0])
	}
	if input.RouteRules[1].TargetNodeID != 21 {
		t.Fatalf("second rule node id = %d, want 21", input.RouteRules[1].TargetNodeID)
	}
	if input.Fallbacks != nil {
		t.Fatalf("fallbacks = %#v, want nil preserved", input.Fallbacks)
	}
}

func TestOperationsConfigRequestInputNilRouteRulesStayNil(t *testing.T) {
	request := operationsConfigRequest{ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300, Fallbacks: nil}
	input, err := request.input()
	if err != nil {
		t.Fatal(err)
	}
	if input.RouteRules != nil {
		t.Fatalf("route rules = %#v, want nil (keep-stored semantics)", input.RouteRules)
	}
	if input.Fallbacks != nil {
		t.Fatalf("fallbacks = %#v, want nil", input.Fallbacks)
	}
}

func TestOperationsConfigRequestInputInvalidRouteRuleNodeID(t *testing.T) {
	request := operationsConfigRequest{
		ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		RouteRules: []operationsRouteRuleRequest{
			{Scope: "grok_build", Class: "billing", TargetMode: "fixed", TargetNodeID: "0", Enabled: true},
		},
	}
	if _, err := request.input(); err == nil {
		t.Fatal("expected error for zero target node id")
	}
}
