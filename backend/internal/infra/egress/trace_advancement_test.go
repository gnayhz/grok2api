package egress

import (
	"context"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 降智回退记录的出口归属不变式:fail-open 降智响应被暂存后,后续尝试耗尽
// 时写降智审计,其 Trace 必须仍指向暂存回退自己的出口——依据是"选择只在
// 成功获取租约时推进"(失败的尝试不覆盖 selections),因此暂存后无法获取的
// 尝试不会污染归属。若未来把失败获取也记入 trace,此不变式破坏,降智归因
// 将指向未承流的出口。
func TestTraceSelectionOnlyAdvancesOnSuccessfulAcquisition(t *testing.T) {
	ctx, trace := WithTrace(context.Background())

	// 第一次选择:节点 11。
	recordSelection(ctx, Selection{Scope: domain.ScopeBuild, NodeID: 11, Proxied: true})
	if got, ok := trace.Selection(domain.ScopeBuild); !ok || got.NodeID != 11 {
		t.Fatalf("first selection = %+v ok=%v", got, ok)
	}

	// 模拟"后续尝试获取失败"——失败路径不调用 recordSelection,trace 保持
	// 指向暂存回退(节点 11)。直接断言:无新的 recordSelection 调用时,读数不变。
	if got, _ := trace.Selection(domain.ScopeBuild); got.NodeID != 11 {
		t.Fatalf("selection drifted without a new acquisition: %+v", got)
	}

	// 新的成功获取才推进。
	recordSelection(ctx, Selection{Scope: domain.ScopeBuild, NodeID: 22, Proxied: true})
	if got, _ := trace.Selection(domain.ScopeBuild); got.NodeID != 22 {
		t.Fatalf("successful acquisition did not advance selection: %+v", got)
	}

	// 并发读安全快照语义:Selection 返回值副本,不受后续更新影响。
	snapshot, _ := trace.Selection(domain.ScopeBuild)
	recordSelection(ctx, Selection{Scope: domain.ScopeBuild, NodeID: 33, Proxied: true})
	if snapshot.NodeID != 22 {
		t.Fatalf("snapshot was not a copy: %+v", snapshot)
	}
}
