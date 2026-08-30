package gateway

import (
	"testing"
	"time"
)

// TestQualityLivenessSchedule：活跃度预算制度表行锁——依据
// 全链路轨迹摸底（33+ 捕获；t3 修订：总静默时证据截止先于首事件截止；
// z2/q2 修订：转换流（chat/messages）的 summary 证据被结构性延迟到
// item.done，证据截止对转换流不适用——q2 low 档无工具被误杀实证）。
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
		if cfg.EvidenceTimeout != qualitySearchSilenceBudget {
			t.Fatal("converted chat: evidence deadline must not apply (deferred-summary window, z2/q2)")
		}
		if cfg.CreatedTimeout != base.CreatedTimeout {
			t.Fatal("converted chat: created deadline stays (created events pass through the converter)")
		}
	}
	{
		cfg := qualityLivenessSchedule([]byte(`{"model":"m"}`), "messages", base)
		if cfg.EvidenceTimeout != qualitySearchSilenceBudget {
			t.Fatal("converted messages: evidence deadline must not apply")
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
		if cfg.EvidenceTimeout != qualitySearchSilenceBudget {
			t.Fatal("object tool_choice without tools: converted evidence row must still apply")
		}
		if cfg.CreatedTimeout != base.CreatedTimeout {
			t.Fatal("object tool_choice without tools: created budget stays")
		}
	}
	{
		// 规模轮 106 实证回归：行序曾让 heavy 压过 converted——chat xhigh 思考
		// >30s 未到 item.done 即被 30s 证据截止误杀（ai5 批次 504，原始流 27 条
		// 思考增量被杀于进行中）。converted 证据无界必须优先；created 保留 30s。
		cfg := qualityLivenessSchedule([]byte(`{"model":"m","reasoning_effort":"xhigh"}`), "chat", base)
		if cfg.EvidenceTimeout != qualitySearchSilenceBudget {
			t.Fatal("chat heavy: converted evidence row must override heavy (deferred visibility)")
		}
		if cfg.CreatedTimeout != qualityHeavyReasoningCreatedBudget {
			t.Fatal("chat heavy: created deadline stays tightened")
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
