package rsc

import (
	"fmt"
	"strings"
	"testing"
)

// realisticUserPayload 复刻 2026-08-24 双账号实测的 grok.com 用户载荷结构
// (匿名化): botFlagSource 与 botFlagDetails 相邻, 邻居字段为 emailConfirmed/
// experienceAcls/canUseDebugTools。ParseRisk 的载荷锚定依赖这些邻居。
func realisticUserPayload(source int, details string) string {
	detailsValue := "null"
	if details != "" {
		detailsValue = "\"" + details + "\""
	}
	return fmt.Sprintf(`{"user":{"email":"u@example.com","emailConfirmed":true,"xSubscriptionType":"","sessionTierId":"2","organizationId":null,"botFlagSource":%d,"botFlagDetails":%s,"experienceAcls":[],"canUseDebugTools":false,"createTime":1786505305}}`, source, detailsValue)
}

func TestAnchoredRealWorldDeniedShape(t *testing.T) {
	body := homepage(realisticUserPayload(1, "policy=deny,risk=0.87,event=$registration"))
	result := ParseRisk(body)
	if result.Verdict != VerdictDenied || result.BotFlagSource != 1 || !result.HasRiskScore || result.RiskScore != 0.87 {
		t.Fatalf("real-world denied = %#v", result)
	}
}

func TestAnchoredRealWorldCleanShape(t *testing.T) {
	body := homepage(realisticUserPayload(0, ""))
	result := ParseRisk(body)
	if result.Verdict != VerdictClean || result.BotFlagSource != 0 {
		t.Fatalf("real-world clean = %#v", result)
	}
}

func TestAnchoredFlaggedSourceWithoutDeny(t *testing.T) {
	body := homepage(realisticUserPayload(2, "castle_token: no_token event=$login source=Web"))
	result := ParseRisk(body)
	if result.Verdict != VerdictFlagged || result.BotFlagSource != 2 {
		t.Fatalf("anchored flagged = %#v", result)
	}
}

// 诱饵防护: botFlagSource 字样出现在非用户载荷上下文(如营销文案)时, 必须
// 判 error(结构异常, 走重试语义)而不是被读成风险状态或 clean。
func TestAnchoredDecoyBotFlagOutsideUserPayloadIsError(t *testing.T) {
	decoy := homepage(`{"marketing":{"copy":"learn about botFlagSource and botFlagDetails today","source":"campaign","emailConfirmed":false}}`)
	_ = decoy
	// 无邻居锚(source 与 details 不相邻且无 experienceAcls/canUseDebugTools)
	result := ParseRisk(homepage(`{"marketing":{"copy":"see botFlagSource docs and botFlagDetails faq","campaign":"spring"}}`))
	if result.Verdict != VerdictError {
		t.Fatalf("decoy fixture = %#v, want error", result)
	}
	if !strings.Contains(result.Error, "payload") {
		t.Fatalf("decoy error should mention payload anchoring: %q", result.Error)
	}
}
