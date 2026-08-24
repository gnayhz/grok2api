package egress

import (
	"context"
	"net/http"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

func swapPhysicalMetrics(t *testing.T) (*perfmetrics.Registry, *perfmetrics.Registry) {
	t.Helper()
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })
	return registry, previous
}

// RecordDirectPhysicalCall 的委托语义:携带 trace 的上下文里,直连物理调用
// 以既有 trace 的 provider/operation 计数,stage 保持 primary、plane 保持
// provider 默认面(直连未注解其他面)。无 trace 的上下文(如 bootstrap)静默
// 不计数——观察面只覆盖被网关显式追踪的请求,不产生 unknown 噪音。
func TestRecordDirectPhysicalCallDelegation(t *testing.T) {
	registry, _ := swapPhysicalMetrics(t)

	ctx := WithPhysicalCallTrace(context.Background(), "grok_build", "responses")
	RecordDirectPhysicalCall(ctx, &http.Response{StatusCode: http.StatusForbidden}, nil)

	samples := registry.CollectAndReset()
	assertPhysicalCallMetric(t, samples, "grok_build", "build", "primary", "1", "client_error")

	// 无 trace:不产生任何计数(含 unknown 标签噪音)。
	bare := registry.CollectAndReset()
	RecordDirectPhysicalCall(context.Background(), nil, nil)
	after := registry.CollectAndReset()
	if len(after) != len(bare) {
		t.Fatalf("traceless direct call produced noise: before=%d after=%d samples", len(bare), len(after))
	}
}
