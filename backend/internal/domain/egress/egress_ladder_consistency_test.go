package egress

import "testing"

// DecidingLevel 与 TargetFor 必须同源:对任意配置组合,归因层级指到的
// 规则必须与 TargetFor 实际命中的规则一致,统计归因不得与路由决策分叉。
func TestDecidingLevelMatchesTargetFor(t *testing.T) {
	nodeTarget := RoutingTarget{Mode: RoutingTargetNode, NodeID: 1}
	directTarget := RoutingTarget{Mode: RoutingTargetDirect}
	classTarget := RoutingTarget{Mode: RoutingTargetNode, NodeID: 3}
	scopeTarget := RoutingTarget{Mode: RoutingTargetPool, PoolID: 4}

	cases := []struct {
		name           string
		config         OperationsConfig
		scope          Scope
		class          TrafficClass
		wantLevel      string
		wantConfigured bool
		wantTarget     RoutingTarget
	}{
		{
			name:           "unconfigured falls to auto",
			config:         OperationsConfig{},
			scope:          ScopeBuild,
			class:          TrafficClassInference,
			wantLevel:      "default",
			wantConfigured: false,
			wantTarget:     RoutingTarget{Mode: RoutingTargetAuto},
		},
		{
			name:           "default only",
			config:         OperationsConfig{DefaultTarget: directTarget},
			scope:          ScopeBuild,
			class:          TrafficClassInference,
			wantLevel:      "default",
			wantConfigured: true,
			wantTarget:     directTarget,
		},
		{
			name: "scope overrides default",
			config: OperationsConfig{
				DefaultTarget: directTarget,
				ScopeTargets:  map[Scope]RoutingTarget{ScopeBuild: scopeTarget},
			},
			scope:          ScopeBuild,
			class:          TrafficClassInference,
			wantLevel:      "scope:grok_build",
			wantConfigured: true,
			wantTarget:     scopeTarget,
		},
		{
			name: "class overrides scope and default",
			config: OperationsConfig{
				DefaultTarget: directTarget,
				ScopeTargets:  map[Scope]RoutingTarget{ScopeBuild: scopeTarget},
				ClassTargets:  map[TrafficClass]RoutingTarget{TrafficClassInference: classTarget},
			},
			scope:          ScopeBuild,
			class:          TrafficClassInference,
			wantLevel:      "class:inference",
			wantConfigured: true,
			wantTarget:     classTarget,
		},
		{
			name: "class for other traffic class does not apply",
			config: OperationsConfig{
				ScopeTargets: map[Scope]RoutingTarget{ScopeWeb: nodeTarget},
				ClassTargets: map[TrafficClass]RoutingTarget{TrafficClassBilling: classTarget},
			},
			scope:          ScopeWeb,
			class:          TrafficClassInference,
			wantLevel:      "scope:grok_web",
			wantConfigured: true,
			wantTarget:     nodeTarget,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, configured := tc.config.DecidingLevel(tc.scope, tc.class)
			if level != tc.wantLevel || configured != tc.wantConfigured {
				t.Fatalf("DecidingLevel = (%q, %v), want (%q, %v)", level, configured, tc.wantLevel, tc.wantConfigured)
			}
			got := tc.config.TargetFor(tc.scope, tc.class)
			if got != tc.wantTarget {
				t.Fatalf("TargetFor = %#v, want %#v", got, tc.wantTarget)
			}
		})
	}
}

// 账号模板判定与"代理池模式节点"判定的唯一权威在 domain:
// 显式标志与模板占位符任一命中即为池模式节点。
func TestIsPoolModeNode(t *testing.T) {
	cases := []struct {
		name     string
		node     Node
		proxyURL string
		want     bool
	}{
		{"neither", Node{}, "http://plain.example:8080", false},
		{"flag only", Node{ProxyPool: true}, "http://plain.example:8080", false},
		{"flag+rotation", Node{ProxyPool: true, RotationEnabled: true}, "http://plain.example:8080", true},
		{"rotation only", Node{RotationEnabled: true}, "http://plain.example:8080", false},
		{"template only", Node{}, "http://gw.example:8080?user={account}", true},
		{"both", Node{ProxyPool: true, RotationEnabled: true}, "http://gw.example:8080?user={account}", true},
		{"flag without rotation no url", Node{ProxyPool: true}, "", false},
		{"rotating no url", Node{ProxyPool: true, RotationEnabled: true}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.node.IsPoolModeNode(tc.proxyURL); got != tc.want {
				t.Fatalf("IsPoolModeNode(%v, %q) = %v, want %v", tc.node.ProxyPool, tc.proxyURL, got, tc.want)
			}
		})
	}
	if !IsAccountTemplateProxy("http://x/?u={account}") || IsAccountTemplateProxy("http://x/") {
		t.Fatal("IsAccountTemplateProxy placeholder detection broken")
	}
}
