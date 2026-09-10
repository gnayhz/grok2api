package gateway

import "testing"

// TestGuardSelfCheck 锁定自检的双向断言:合成的"reasoning item 闭合+零思考
// 增量+usage 谎报非零 reasoning tokens"降智流必须 withhold,可见 summary
// 增量的干净流必须 deliver。自检在启动时执行,此测试防其自身回归。
func TestGuardSelfCheck(t *testing.T) {
	if err := GuardSelfCheck(); err != nil {
		t.Fatalf("guard self check failed: %v", err)
	}
}
