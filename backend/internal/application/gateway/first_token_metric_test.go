package gateway

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// TestFirstTokenMetricRecordsIntoRegistry 锁定 TTFT 直方图契约:finalize
// 打点(first_token_us)按 Subsystem+Provider 有界标签进入全局注册表,
// 可经 CollectAndReset 读取——链路优化的效果可实时观测,不再只能查审计库。
func TestFirstTokenMetricRecordsIntoRegistry(t *testing.T) {
	perfmetrics.Default.ObserveDuration("first_token_us", perfmetrics.Labels{Subsystem: "gateway", Provider: "grok_build"}, 1500*time.Millisecond)
	found := false
	for _, sample := range perfmetrics.Default.CollectAndReset() {
		if sample.Name == "first_token_us" && sample.Labels.Subsystem == "gateway" && sample.Labels.Provider == "grok_build" && sample.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("first_token_us sample missing from registry after observation")
	}
}
