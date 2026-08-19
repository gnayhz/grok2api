package egress

import "testing"

func TestTrafficClassIsValid(t *testing.T) {
	for _, class := range []TrafficClass{TrafficClassInference, TrafficClassCredential, TrafficClassBilling, TrafficClassModelSync, TrafficClassVideo} {
		if !class.IsValid() {
			t.Errorf("expected %q to be valid", class)
		}
	}
	for _, class := range []TrafficClass{"", "auxiliary", "INFERENCE", "unknown"} {
		if class.IsValid() {
			t.Errorf("expected %q to be invalid", class)
		}
	}
}

func TestTrafficClassRespectsAccountBinding(t *testing.T) {
	binding := map[TrafficClass]bool{
		TrafficClassInference:  true,
		TrafficClassVideo:      true,
		TrafficClassCredential: false,
		TrafficClassBilling:    false,
		TrafficClassModelSync:  false,
	}
	for class, want := range binding {
		if got := class.RespectsAccountBinding(); got != want {
			t.Errorf("%s RespectsAccountBinding = %v, want %v", class, got, want)
		}
	}
}

func TestRouteRuleTargetModeNormalized(t *testing.T) {
	if got := RouteRuleTargetMode("").Normalized(); got != RouteRuleTargetFixed {
		t.Errorf("empty mode normalized to %q, want fixed", got)
	}
	if got := RouteRuleTargetMode("direct").Normalized(); got != RouteRuleTargetDirect {
		t.Errorf("direct mode normalized to %q, want direct", got)
	}
}

func TestOperationsConfigRouteRuleFor(t *testing.T) {
	config := OperationsConfig{
		RouteRules: []RouteRule{
			{Scope: ScopeBuild, Class: TrafficClassInference, TargetMode: RouteRuleTargetFixed, TargetNodeID: 7, Enabled: true},
			{Scope: ScopeBuild, Class: TrafficClassBilling, TargetMode: RouteRuleTargetDirect, Enabled: true},
			{Scope: ScopeBuild, Class: TrafficClassModelSync, TargetMode: RouteRuleTargetFixed, TargetNodeID: 0, Enabled: true},
			{Scope: ScopeBuild, Class: TrafficClassCredential, TargetMode: RouteRuleTargetFixed, TargetNodeID: 9, Enabled: false},
		},
	}
	if rule, ok := config.RouteRuleFor(ScopeBuild, TrafficClassInference); !ok || rule.TargetNodeID != 7 {
		t.Errorf("inference rule = %+v ok=%v, want node 7", rule, ok)
	}
	if rule, ok := config.RouteRuleFor(ScopeBuild, TrafficClassBilling); !ok || rule.TargetMode != RouteRuleTargetDirect {
		t.Errorf("billing rule = %+v ok=%v, want direct", rule, ok)
	}
	// A fixed rule without a node id is treated as absent rather than routed to node 0.
	if _, ok := config.RouteRuleFor(ScopeBuild, TrafficClassModelSync); ok {
		t.Error("model_sync rule with zero node id should not match")
	}
	// Disabled rules never match.
	if _, ok := config.RouteRuleFor(ScopeBuild, TrafficClassCredential); ok {
		t.Error("disabled rule should not match")
	}
	// Unknown scope or class never matches.
	if _, ok := config.RouteRuleFor(ScopeWeb, TrafficClassBilling); ok {
		t.Error("rule for another scope should not match")
	}
	if _, ok := config.RouteRuleFor(ScopeBuild, TrafficClassVideo); ok {
		t.Error("unconfigured class should not match")
	}
}

func TestRouteRuleClasses(t *testing.T) {
	if classes := RouteRuleClasses(ScopeBuild); len(classes) != 5 {
		t.Errorf("build classes = %v, want 5 entries", classes)
	}
	for _, scope := range []Scope{ScopeWeb, ScopeConsole, ScopeWebAsset, ScopeConsoleAsset} {
		if classes := RouteRuleClasses(scope); classes != nil {
			t.Errorf("scope %s should not support route rules yet, got %v", scope, classes)
		}
	}
}

func TestValidateRouteRules(t *testing.T) {
	valid := []RouteRule{
		{Scope: ScopeBuild, Class: TrafficClassInference, TargetMode: RouteRuleTargetFixed, TargetNodeID: 1, Enabled: true},
		{Scope: ScopeBuild, Class: TrafficClassCredential, TargetMode: RouteRuleTargetDirect, Enabled: true},
	}
	if err := ValidateRouteRules(valid); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	if err := ValidateRouteRules(nil); err != nil {
		t.Fatalf("empty rules rejected: %v", err)
	}

	cases := []struct {
		name string
		rule RouteRule
	}{
		{"non-build scope", RouteRule{Scope: ScopeWeb, Class: TrafficClassBilling, TargetMode: RouteRuleTargetDirect, Enabled: true}},
		{"invalid class", RouteRule{Scope: ScopeBuild, Class: TrafficClass("other"), TargetMode: RouteRuleTargetDirect, Enabled: true}},
		{"fixed without node", RouteRule{Scope: ScopeBuild, Class: TrafficClassBilling, TargetMode: RouteRuleTargetFixed, Enabled: true}},
		{"invalid target mode", RouteRule{Scope: ScopeBuild, Class: TrafficClassBilling, TargetMode: RouteRuleTargetMode("pool"), Enabled: true}},
		{"direct with node", RouteRule{Scope: ScopeBuild, Class: TrafficClassBilling, TargetMode: RouteRuleTargetDirect, TargetNodeID: 3, Enabled: true}},
	}
	for _, testCase := range cases {
		if err := ValidateRouteRules([]RouteRule{testCase.rule}); err == nil {
			t.Errorf("%s: expected validation error", testCase.name)
		}
	}

	duplicate := []RouteRule{
		{Scope: ScopeBuild, Class: TrafficClassBilling, TargetMode: RouteRuleTargetDirect, Enabled: true},
		{Scope: ScopeBuild, Class: TrafficClassBilling, TargetMode: RouteRuleTargetFixed, TargetNodeID: 2, Enabled: false},
	}
	if err := ValidateRouteRules(duplicate); err == nil {
		t.Error("duplicate (scope, class) should be rejected even when one rule is disabled")
	}

	// Explicit count cap fails fast before per-rule node lookups.
	oversized := make([]RouteRule, MaxRouteRules+1)
	for index := range oversized {
		oversized[index] = RouteRule{Scope: ScopeBuild, Class: TrafficClassInference, TargetMode: RouteRuleTargetDirect, Enabled: true}
	}
	if err := ValidateRouteRules(oversized); err == nil {
		t.Error("oversized rule list should be rejected by the count cap")
	}
}
