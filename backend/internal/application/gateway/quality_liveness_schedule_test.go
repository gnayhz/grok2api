package gateway

import (
	"testing"
	"time"
)

// TestQualityLivenessSchedule：活跃度预算制度表行锁。
// 守卫已改为在转换器之前看原始 SSE，chat/messages 与 native responses
// 共用同一套 created/evidence 预算（无工具=默认，搜索=无界，heavy=30s）。
func TestQualityLivenessSchedule(t *testing.T) {
	t.Parallel()
	wantModels := []string{"grok-4.5", "grok-4.6"}
	base := QualityRetryRuntime{Enabled: true, CreatedTimeout: 5 * time.Second, EvidenceTimeout: 3500 * time.Millisecond, GuardedModels: wantModels}
	assertScope := func(cfg QualityRetryRuntime) {
		t.Helper()
		if len(cfg.GuardedModels) != 2 || cfg.GuardedModels[0] != wantModels[0] || cfg.GuardedModels[1] != wantModels[1] {
			t.Fatalf("GuardedModels = %#v, want %#v", cfg.GuardedModels, wantModels)
		}
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","tools":[{"type":"web_search"}]}`), "responses", base)
		if cfg.CreatedTimeout != qualitySearchSilenceBudget || cfg.EvidenceTimeout != qualitySearchSilenceBudget {
			t.Fatal("search: both budgets must be unbounded")
		}
		assertScope(cfg)
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","reasoning":{"effort":"high"}}`), "responses", base)
		if cfg.CreatedTimeout != qualityHeavyReasoningCreatedBudget || cfg.EvidenceTimeout != qualityHeavyReasoningCreatedBudget {
			t.Fatal("heavy: both budgets must scale (t3 trace)")
		}
		assertScope(cfg)
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m"}`), "chat", base)
		if cfg.EvidenceTimeout != base.EvidenceTimeout || cfg.CreatedTimeout != base.CreatedTimeout {
			t.Fatal("chat without tools: raw peek uses default budgets")
		}
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m"}`), "messages", base)
		if cfg.EvidenceTimeout != base.EvidenceTimeout || cfg.CreatedTimeout != base.CreatedTimeout {
			t.Fatal("messages without tools: raw peek uses default budgets")
		}
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","tools":[{"type":"web_search"}],"tool_choice":"none"}`), "responses", base)
		// 结构体含 []string 不可整体比较；该用例只关心预算回到默认行。
		if cfg.EvidenceTimeout != base.EvidenceTimeout || cfg.CreatedTimeout != base.CreatedTimeout {
			t.Fatal("disabled tools (choice none): default budgets apply")
		}
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","tools":[{"type":"web_search"},{"type":"x_search"}],"tool_choice":"none"}`), "chat", base)
		if cfg.EvidenceTimeout != base.EvidenceTimeout || cfg.CreatedTimeout != base.CreatedTimeout {
			t.Fatal("injected web_search+x_search with choice none: default budgets, not search silence")
		}
	}
	{
		// 规模轮 76 实证回归：forced 对象形态曾使 string 字段探测整体失败，
		// 回退默认行——chat 请求连 converted 无界证据行都落空，搜索静默期
		// 3.5s 误杀 504（f6 批次，可复现）。
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","tools":[{"type":"web_search"}],"tool_choice":{"type":"tool","name":"web_search"}}`), "chat", base)
		if cfg.CreatedTimeout != qualitySearchSilenceBudget || cfg.EvidenceTimeout != qualitySearchSilenceBudget {
			t.Fatal("forced object tool_choice: probe must not fail, any-tools row applies")
		}
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","tool_choice":{"type":"tool","name":"web_search"}}`), "chat", base)
		if cfg.EvidenceTimeout != base.EvidenceTimeout || cfg.CreatedTimeout != base.CreatedTimeout {
			t.Fatal("object tool_choice without tools: no tools array, default budgets")
		}
	}
	{
		// chat xhigh：与 native responses 同用 30s created/evidence（排队 >5s）。
		// 思考增量仍 0ms 放行；item.done 无思考仍 0ms 扣留。证据截止只收静默。
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","reasoning_effort":"xhigh"}`), "chat", base)
		if cfg.EvidenceTimeout != qualityHeavyReasoningCreatedBudget || cfg.CreatedTimeout != qualityHeavyReasoningCreatedBudget {
			t.Fatal("chat heavy: raw peek uses the same heavy budgets as native responses")
		}
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m"}`), "responses", base)
		// QualityRetryRuntime 含 []string 字段不可整体比较，按语义比较预算
		// 与范围字段（schedule 不改动其余字段）。
		if cfg.EvidenceTimeout != base.EvidenceTimeout || cfg.CreatedTimeout != base.CreatedTimeout {
			t.Fatal("native responses default: budgets unchanged")
		}
		assertScope(cfg)
	}
}
