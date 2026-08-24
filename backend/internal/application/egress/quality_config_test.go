package egress

import (
	"testing"
	"time"
)

// 守卫配置归一化:零/负时长回落默认(文件级校验拒绝越界,这里是程序化构造的
// 兜底);正时长原样通过;CrossAccountThreshold 刻意不归一(0/1=关闭跨账号
// 兜底,是文档化语义而非缺省)。
func TestQualityGuardConfigNormalization(t *testing.T) {
	// 全零 → 时长全默认;阈值刻意保持 0(文件层 DefaultEgressConfig 预填 2,
	// yaml 部分段保留兄弟默认——由 config 层测试锁定;程序化构造 0 表达
	// "关闭跨账号兜底"是使用点 <2 判定的文档化语义)。
	zero := QualityGuardConfig{}.normalized()
	def := DefaultQualityGuardConfig()
	if zero.QuarantineCooldown != def.QuarantineCooldown || zero.CrossAccountWindow != def.CrossAccountWindow || zero.TentativeReleaseCooldown != def.TentativeReleaseCooldown {
		t.Fatalf("zero durations not defaulted: %+v", zero)
	}
	if zero.CrossAccountThreshold != 0 {
		t.Fatalf("threshold coerced to %d: 归一化不得改写阈值(0=关闭兜底的显式语义)", zero.CrossAccountThreshold)
	}

	// 负值(程序化构造的畸形)同样回落默认,不产生负冷却。
	negative := QualityGuardConfig{
		QuarantineCooldown:       -time.Hour,
		CrossAccountWindow:       -time.Minute,
		TentativeReleaseCooldown: -time.Second,
	}.normalized()
	if negative.QuarantineCooldown != def.QuarantineCooldown || negative.CrossAccountWindow != def.CrossAccountWindow || negative.TentativeReleaseCooldown != def.TentativeReleaseCooldown {
		t.Fatalf("negative durations not defaulted: %+v", negative)
	}

	// 显式正值原样通过——不被默认覆盖。
	explicit := QualityGuardConfig{
		QuarantineCooldown:       2 * time.Hour,
		CrossAccountThreshold:    5,
		CrossAccountWindow:       10 * time.Minute,
		TentativeReleaseCooldown: 7 * time.Minute,
	}.normalized()
	if explicit.QuarantineCooldown != 2*time.Hour || explicit.CrossAccountThreshold != 5 ||
		explicit.CrossAccountWindow != 10*time.Minute || explicit.TentativeReleaseCooldown != 7*time.Minute {
		t.Fatalf("explicit values not preserved: %+v", explicit)
	}

	// 阈值刻意不归一:0 与 1 都保持原值(使用点以 <2 判关闭),不抬到默认 2。
	for _, threshold := range []int{0, 1} {
		got := QualityGuardConfig{CrossAccountThreshold: threshold}.normalized().CrossAccountThreshold
		if got != threshold {
			t.Fatalf("threshold %d normalized to %d: 低于 2 是文档化的关闭语义,不得被改写", threshold, got)
		}
	}
}
