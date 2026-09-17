package egress

import (
	"testing"
	"time"
)

// 冷却口径的唯一判定:池模式端点豁免普通冷却(服务商换一个出口即失效),
// 但出口 IP 质量隔离(LastErrorExitIPQuality)指向端点自身的降智问题,对
// 池模式同样生效。自动调度、池成员过滤与固定目标三条路径曾各写一份,固定
// 目标副本整段绕过隔离,被隔离的降智出口借"旋转"名义继续承流。
func TestCooldownBlocksScheduling(t *testing.T) {
	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	active := now.Add(time.Minute)
	expired := now.Add(-time.Minute)
	cases := []struct {
		name     string
		state    HealthState
		poolMode bool
		want     bool
	}{
		{"no cooldown", HealthState{Health: 1}, false, false},
		{"no cooldown pool mode", HealthState{Health: 1}, true, false},
		{"expired transport cooldown", HealthState{CooldownUntil: &expired, LastError: LastErrorTransport}, false, false},
		{"expired quality quarantine", HealthState{CooldownUntil: &expired, LastError: LastErrorExitIPQuality}, true, false},
		{"active transport cooldown blocks fixed exit", HealthState{CooldownUntil: &active, LastError: LastErrorTransport}, false, true},
		{"active transport cooldown exempts pool mode", HealthState{CooldownUntil: &active, LastError: LastErrorTransport}, true, false},
		{"active cooldown without reason exempts pool mode", HealthState{CooldownUntil: &active}, true, false},
		{"active quality quarantine blocks fixed exit", HealthState{CooldownUntil: &active, LastError: LastErrorExitIPQuality}, false, true},
		{"active quality quarantine blocks pool mode", HealthState{CooldownUntil: &active, LastError: LastErrorExitIPQuality}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CooldownBlocksScheduling(tc.state.CooldownUntil, tc.state.LastError, tc.poolMode, now); got != tc.want {
				t.Fatalf("CooldownBlocksScheduling(%+v, poolMode=%v) = %v, want %v", tc.state, tc.poolMode, got, tc.want)
			}
		})
	}
}

// 旋转端点(池模式)的健康投影是唯一实现:满健康、无失败计数、无冷却、
// 无错误原因;它只清这一份投影,存储/反馈链路上的真实健康状态与 revision
// 原样保留。管理端列表与自动调度共用它,防止两处各写一份字面量。
func TestRotatingEndpointHealthClearsPenaltiesOnlyInProjection(t *testing.T) {
	until := time.Date(2024, 5, 1, 12, 30, 0, 0, time.UTC)
	state := HealthState{Revision: 7, Health: 0.05, FailureCount: 9, CooldownUntil: &until, LastError: LastErrorExitIPQuality}
	projected := RotatingEndpointHealth(state)
	if projected.Revision != 7 {
		t.Fatalf("projection changed the health revision: %d", projected.Revision)
	}
	if projected.Health != 1 || projected.FailureCount != 0 || projected.CooldownUntil != nil || projected.LastError != "" {
		t.Fatalf("projection kept obsolete health: %+v", projected)
	}
	if state.Health != 0.05 || state.FailureCount != 9 || state.CooldownUntil == nil || state.LastError != LastErrorExitIPQuality {
		t.Fatalf("projection mutated the source state: %+v", state)
	}
}
