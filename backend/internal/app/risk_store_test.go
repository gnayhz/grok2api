package app

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/account/risk"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// 契约:网关 Build 探针的每一个导出 outcome 都必须映射到风险侧四个合法
// verdict 之一。曾因网关词汇 degraded 不在适配器白名单内、被 default 改写
// 成 error,差分定罪链路(buildProbe 的存在意义)整体失效——本测试锁死
// 词汇映射,新增 outcome 常量而忘记映射时在此失败。
func TestBuildProbeOutcomeVerdictContract(t *testing.T) {
	cases := map[string]string{
		gateway.BuildProbeOutcomeClean:        risk.BuildProbeClean,
		gateway.BuildProbeOutcomeDegraded:     risk.BuildProbeDenied,
		gateway.BuildProbeOutcomeError:        risk.BuildProbeError,
		gateway.BuildProbeOutcomeUnconfigured: risk.BuildProbeUnconfigured,
	}
	for outcome, want := range cases {
		if got := buildProbeOutcomeVerdict(outcome); got != want {
			t.Errorf("buildProbeOutcomeVerdict(%q) = %q, want %q", outcome, got, want)
		}
	}
	// 未知词 fail-safe 到 error,绝不猜结论。
	if got := buildProbeOutcomeVerdict("mystery"); got != risk.BuildProbeError {
		t.Errorf("buildProbeOutcomeVerdict(unknown) = %q, want %q", got, risk.BuildProbeError)
	}
	// degraded→denied 是本契约的核心:定罪路径必须可达。
	if got := buildProbeOutcomeVerdict(gateway.BuildProbeOutcomeDegraded); got != risk.BuildProbeDenied {
		t.Fatalf("double-degraded must map to denied (conviction), got %q", got)
	}
}

func TestRiskStoreMapsDeniedStreak(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	in := risk.StoredVerdict{
		Verdict: risk.VerdictDenied, BotFlagDtl: "dtl", HTTPStatus: 200,
		Error: "", Source: "sso_probe", CheckedAt: now,
		OriginAccountID: 90, Trigger: "patrol", DeniedStreak: 2,
	}
	row := relationalFromStoredVerdict(90, in)
	if row.AccountID != 90 || row.DeniedStreak != 2 || row.Verdict != risk.VerdictDenied || row.Trigger != "patrol" {
		t.Fatalf("row = %+v", row)
	}
	out := storedVerdictFromRow(row)
	if out.DeniedStreak != 2 || out.OriginAccountID != 90 || out.Trigger != "patrol" || out.Source != "sso_probe" {
		t.Fatalf("roundtrip dropped fields: %+v", out)
	}
	zero := storedVerdictFromRow(relational.AccountRiskVerdict{Verdict: "denied"})
	if zero.DeniedStreak != 0 {
		t.Fatalf("legacy row streak = %d, want 0", zero.DeniedStreak)
	}
}

// 词汇映射的结果必须全部落在风险侧 BuildProber 的合法 verdict 集内,
// 否则 checkNowBuild 的 switch 会把结论静默降级为 error。
func TestBuildProbeOutcomeVerdictStaysInRiskVocabulary(t *testing.T) {
	valid := map[string]bool{
		risk.BuildProbeClean: true, risk.BuildProbeDenied: true,
		risk.BuildProbeError: true, risk.BuildProbeUnconfigured: true,
	}
	for _, outcome := range []string{
		gateway.BuildProbeOutcomeClean, gateway.BuildProbeOutcomeDegraded,
		gateway.BuildProbeOutcomeError, gateway.BuildProbeOutcomeUnconfigured,
		"", "denied", "garbage",
	} {
		if got := buildProbeOutcomeVerdict(outcome); !valid[got] {
			t.Errorf("buildProbeOutcomeVerdict(%q) = %q, outside risk vocabulary", outcome, got)
		}
	}
}
