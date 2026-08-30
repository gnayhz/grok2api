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
	base := QualityRetryRuntime{Enabled: true, CreatedTimeout: 5 * time.Second, EvidenceTimeout: 3500 * time.Millisecond}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","tools":[{"type":"web_search"}]}`), "responses", base)
		if cfg.CreatedTimeout != qualitySearchSilenceBudget || cfg.EvidenceTimeout != qualitySearchSilenceBudget {
			t.Fatal("search: both budgets must be unbounded")
		}
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","reasoning":{"effort":"high"}}`), "responses", base)
		if cfg.CreatedTimeout != qualityHeavyReasoningCreatedBudget || cfg.EvidenceTimeout != qualityHeavyReasoningCreatedBudget {
			t.Fatal("heavy: both budgets must scale (t3 trace)")
		}
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
		// 规模轮 106 实证回归：行序曾让 heavy 压过 converted——chat xhigh 思考
		// >30s 未到 item.done 即被 30s 证据截止误杀（ai5 批次 504，原始流 27 条
		// 思考增量被杀于进行中）。converted 证据无界必须优先；created 保留 30s。
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
		if len(cfg.GuardedModels) != len(base.GuardedModels) {
			t.Fatal("native responses default: scope unchanged")
		}
	}
}
