package egress

import "testing"

func TestRoutingScopeMergesAssetsIntoParentFamily(t *testing.T) {
	tests := []struct {
		scope Scope
		want  Scope
	}{
		{ScopeBuild, ScopeBuild},
		{ScopeWeb, ScopeWeb},
		{ScopeConsole, ScopeConsole},
		{ScopeWebAsset, ScopeWeb},
		{ScopeConsoleAsset, ScopeConsole},
	}
	for _, test := range tests {
		t.Run(string(test.scope), func(t *testing.T) {
			if got := RoutingScope(test.scope); got != test.want {
				t.Fatalf("RoutingScope(%q) = %q, want %q", test.scope, got, test.want)
			}
		})
	}
}

func TestRequestScopesAndTrafficClasses(t *testing.T) {
	scopes := RequestScopes()
	if len(scopes) != 5 {
		t.Fatalf("RequestScopes() = %v, want 5 entries", scopes)
	}
	classes := TrafficClasses()
	if len(classes) != 5 {
		t.Fatalf("TrafficClasses() = %v, want 5 entries", classes)
	}
	for _, class := range classes {
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

func TestOperationsConfigTargetFor(t *testing.T) {
	config := OperationsConfig{
		DefaultTarget: RoutingTarget{Mode: RoutingTargetDirect},
		ScopeTargets: map[Scope]RoutingTarget{
			ScopeBuild: {Mode: RoutingTargetNode, NodeID: 7},
			ScopeWeb:   {Mode: RoutingTargetAuto},
		},
		ClassTargets: map[TrafficClass]RoutingTarget{
			TrafficClassBilling: {Mode: RoutingTargetPool, PoolID: 3},
		},
	}

	// Class level wins over scope and default.
	if got := config.TargetFor(ScopeBuild, TrafficClassBilling); got.Mode != RoutingTargetPool || got.PoolID != 3 {
		t.Errorf("class override = %+v, want pool 3", got)
	}
	// Scope level wins over default.
	if got := config.TargetFor(ScopeBuild, TrafficClassInference); got.Mode != RoutingTargetNode || got.NodeID != 7 {
		t.Errorf("scope override = %+v, want node 7", got)
	}
	// Empty class still uses the scope target.
	if got := config.TargetFor(ScopeBuild, ""); got.NodeID != 7 {
		t.Errorf("empty class should still use scope target, got %+v", got)
	}
	// Default target applies for unconfigured scope.
	if got := config.TargetFor(ScopeConsole, TrafficClassVideo); got.Mode != RoutingTargetDirect {
		t.Errorf("default target = %+v, want direct", got)
	}
	// Explicit auto scope beats the default.
	if got := config.TargetFor(ScopeWeb, TrafficClassInference); got.Mode != RoutingTargetAuto {
		t.Errorf("explicit auto scope = %+v, want auto", got)
	}
}

func TestOperationsConfigTargetForUnconfiguredFallsBackToAuto(t *testing.T) {
	config := OperationsConfig{}
	got := config.TargetFor(ScopeWebAsset, TrafficClassVideo)
	if got.Mode != RoutingTargetAuto {
		t.Errorf("empty config resolved to %+v, want auto", got)
	}
}

func TestRoutingTargetValidity(t *testing.T) {
	valid := []RoutingTarget{
		{},
		{Mode: RoutingTargetAuto},
		{Mode: RoutingTargetDirect},
		{Mode: RoutingTargetNode, NodeID: 1},
		{Mode: RoutingTargetPool, PoolID: 2},
	}
	for _, target := range valid {
		if !target.Valid() {
			t.Errorf("expected %+v to be valid", target)
		}
	}
	invalid := []RoutingTarget{
		{Mode: RoutingTargetNode},
		{Mode: RoutingTargetNode, NodeID: 1, PoolID: 2},
		{Mode: RoutingTargetPool},
		{Mode: RoutingTargetAuto, NodeID: 1},
		{Mode: RoutingTargetDirect, PoolID: 1},
		{Mode: RoutingTargetMode("bogus"), NodeID: 1},
	}
	for _, target := range invalid {
		if target.Valid() {
			t.Errorf("expected %+v to be invalid", target)
		}
	}
	var zeroTarget RoutingTarget
	if zeroTarget.Configured() {
		t.Error("zero target should be unconfigured")
	}
	if got := RoutingTargetMode("").Normalized(); got != RoutingTargetAuto {
		t.Errorf("empty mode normalized to %q, want auto", got)
	}
	resolved := RoutingTarget{Mode: RoutingTargetMode("bogus")}.Resolved()
	if resolved.Mode != RoutingTargetAuto {
		t.Errorf("resolved bogus mode = %q, want auto", resolved.Mode)
	}
}

func TestValidateRoutingTargets(t *testing.T) {
	validScopes := map[Scope]RoutingTarget{
		ScopeBuild:   {Mode: RoutingTargetNode, NodeID: 1},
		ScopeWeb:     {Mode: RoutingTargetDirect},
		ScopeConsole: {Mode: RoutingTargetAuto},
	}
	validClasses := map[TrafficClass]RoutingTarget{
		TrafficClassBilling: {Mode: RoutingTargetPool, PoolID: 2},
	}
	if err := ValidateRoutingTargets(RoutingTarget{Mode: RoutingTargetDirect}, validScopes, validClasses); err != nil {
		t.Fatalf("valid targets rejected: %v", err)
	}
	if err := ValidateRoutingTargets(RoutingTarget{}, nil, nil); err != nil {
		t.Fatalf("empty targets rejected: %v", err)
	}

	cases := []struct {
		name    string
		def     RoutingTarget
		scopes  map[Scope]RoutingTarget
		classes map[TrafficClass]RoutingTarget
	}{
		{"invalid default", RoutingTarget{Mode: RoutingTargetNode}, nil, nil},
		{"asset scope key", RoutingTarget{}, map[Scope]RoutingTarget{ScopeWebAsset: {Mode: RoutingTargetDirect}}, nil},
		{"invalid class key", RoutingTarget{}, nil, map[TrafficClass]RoutingTarget{TrafficClass("other"): {Mode: RoutingTargetDirect}}},
		{"scope node without id", RoutingTarget{}, map[Scope]RoutingTarget{ScopeBuild: {Mode: RoutingTargetNode}}, nil},
		{"class direct with pool", RoutingTarget{}, nil, map[TrafficClass]RoutingTarget{TrafficClassVideo: {Mode: RoutingTargetDirect, PoolID: 3}}},
		{"unconfigured scope entry", RoutingTarget{}, map[Scope]RoutingTarget{ScopeWeb: {}}, nil},
	}
	for _, testCase := range cases {
		if err := ValidateRoutingTargets(testCase.def, testCase.scopes, testCase.classes); err == nil {
			t.Errorf("%s: expected validation error", testCase.name)
		}
	}
}

func TestPoolStrategyNormalized(t *testing.T) {
	for _, strategy := range []PoolStrategy{PoolStrategyAffinity, PoolStrategyRandom, PoolStrategySticky} {
		if !strategy.IsValid() {
			t.Errorf("expected %q valid", strategy)
		}
	}
	if PoolStrategy("").IsValid() {
		t.Error("empty strategy should be invalid")
	}
	if got := PoolStrategy("").Normalized(); got != PoolStrategyAffinity {
		t.Errorf("zero strategy normalized to %q, want affinity (legacy rendezvous)", got)
	}
	if got := PoolFallbackMode("").Normalized(); got != PoolFallbackNone {
		t.Errorf("zero fallback normalized to %q, want none", got)
	}
}

func TestCanNodeServeFixedTarget(t *testing.T) {
	base := Node{Enabled: true, EncryptedProxyURL: "secret"}
	if !CanNodeServeFixedTarget(base) {
		t.Error("enabled node with proxy should serve fixed target")
	}
	disabled := base
	disabled.Enabled = false
	if CanNodeServeFixedTarget(disabled) {
		t.Error("disabled node must not serve fixed target")
	}
	noProxy := base
	noProxy.EncryptedProxyURL = ""
	if CanNodeServeFixedTarget(noProxy) {
		t.Error("node without proxy must not serve fixed target")
	}
	pooled := base
	pooled.ProxyPool = true
	if CanNodeServeFixedTarget(pooled) {
		t.Error("proxy-pool node must not serve fixed target")
	}
}
