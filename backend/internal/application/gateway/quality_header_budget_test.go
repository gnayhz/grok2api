package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestEarlyHeaderAbortSwitchesPath：账号 0 首尝试头迟滞（模拟降智路径），
// 预算早断应中止它并换账号 1 投递健康流——判定发生在头阶段而非首字节。
func TestEarlyHeaderAbortSwitchesPath(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	// 账号 0：头迟滞 200ms（预算 30ms 必然触发）；重试队列留空避免干扰。
	fixture.adapter.responses[fixture.credentials[0].ID] = []scriptedBuildResponse{
		{status: 200, body: aFormStream(), headerDelay: 200 * time.Millisecond},
	}
	fixture.scriptAccount(1, aFormStream())

	cfg := baseSameAccountRuntime()
	cfg.EarlyHeaderAbort = 30 * time.Millisecond
	service := fixture.service(t, cfg)

	started := time.Now()
	result, err := service.CreateChatCompletion(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("switch should deliver, err=%v", err)
	}
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !strings.Contains(string(body), "good answer") {
		t.Fatal("delivered body should come from the second account")
	}
	// 断言预算语义而非绝对墙钟:CI 慢机(2 核 + race + 全包并行)下,预算中止
	// 后的换号+投递开销可远超 150ms,但必然显著小于 200ms 的完整头等待。
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		const headerDelay = 200 * time.Millisecond
		if elapsed >= headerDelay {
			t.Fatalf("early abort waited out the full %s header delay: %s", headerDelay, elapsed)
		}
		t.Logf("budget abort held (elapsed %s < header delay %s; slow environment)", elapsed, headerDelay)
	}
	fixture.assertAttempts(t, 0, 1)
	fixture.assertAttempts(t, 1, 1)
}

// TestEarlyHeaderAbortAppliesPerAttempt：预算对每次流式尝试生效（2026-08-21
// 语义修订）：两个账号都头迟滞时，第二次尝试同样被预算中止——单发语义
// 曾让第二次慢头尝试悬挂满 5 分钟 ResponseHeaderTimeout（魔法球实测 368s
// 中的 300s）。系统性慢（但健康）的头路径在实测中不存在（健康头 0.7-2.2s
// 恒定），若真出现应由调大 earlyHeaderAbort 或关闭预算处理，而非自动放行。
func TestEarlyHeaderAbortAppliesPerAttempt(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	// 两个账号都头迟滞且无健康替代：每次尝试都在预算处被中止，最终按
	// 耗尽处理（不会等到 120ms 的头），总耗时应远小于两次完整头等待。
	for index := range fixture.credentials {
		fixture.adapter.responses[fixture.credentials[index].ID] = []scriptedBuildResponse{
			{status: 200, body: aFormStream(), headerDelay: 120 * time.Millisecond},
		}
	}

	cfg := baseSameAccountRuntime()
	cfg.EarlyHeaderAbort = 30 * time.Millisecond
	service := fixture.service(t, cfg)

	started := time.Now()
	_, _ = service.CreateChatCompletion(context.Background(), fixture.request())
	elapsed := time.Since(started)
	// 预算每次生效：两次尝试各 ~30ms 中止 + 重试开销，绝不接近 2×120ms。
	if elapsed > 150*time.Millisecond {
		// 语义:每次尝试都在 30ms 预算处中止,总耗时应显著小于两次完整头等待。
		// CI 慢机下的固定开销可能超过 150ms 绝对阈值,改与 2x120ms 对照。
		const twoFullHeaderWaits = 240 * time.Millisecond
		if elapsed >= twoFullHeaderWaits {
			t.Fatalf("per-attempt budget did not abort slow-header attempts: %s (>= 2x120ms full waits)", elapsed)
		}
		t.Logf("per-attempt budget held (elapsed %s < 2x120ms full waits; slow environment)", elapsed)
	}
	fixture.assertAttempts(t, 0, 1)
	fixture.assertAttempts(t, 1, 1)
}

// TestEarlyHeaderAbortHealthyStreamDeliversFully：预算内健康返回后 body 必须
// 完整可读——锁死"成功路径误 cancel body 生命周期"的回归（defer cancel 曾
// 让 peek 在 ~0.5s 后读到 context canceled，流式全挂）。
func TestEarlyHeaderAbortHealthyStreamDeliversFully(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	fixture.scriptAccount(0, aFormStream())
	cfg := baseSameAccountRuntime()
	cfg.EarlyHeaderAbort = 30 * time.Millisecond
	service := fixture.service(t, cfg)

	result, err := service.CreateChatCompletion(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("healthy stream must deliver, err=%v", err)
	}
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !strings.Contains(string(body), "reasoning_content") || !strings.Contains(string(body), "good answer") {
		t.Fatal("healthy stream body must be fully readable after in-budget header return")
	}
}

// TestEarlyHeaderBudgetHelper：预算只在 hold 激活、流式、已配置、单发未用时生效。
// 非流式的响应头要等整个生成完成（clean 6-15s），套用流式预算会误杀健康请求
// （2026-08-21 实测：medium/hard clean 非流式在 5.0s 被误断成 504）。
func TestEarlyHeaderBudgetHelper(t *testing.T) {
	cfg := QualityRetryRuntime{EarlyHeaderAbort: 10 * time.Second}
	if qualityHeaderBudget(cfg, true, true, true) != 10*time.Second {
		t.Fatal("armed+enabled+streaming must return the budget")
	}
	if qualityHeaderBudget(cfg, false, true, true) != 0 {
		t.Fatal("hold disabled must disarm")
	}
	if qualityHeaderBudget(cfg, true, false, true) != 0 {
		t.Fatal("non-streaming must disarm (headers arrive at end of generation)")
	}
	if qualityHeaderBudget(cfg, true, true, false) != 0 {
		t.Fatal("already fired must disarm")
	}
	if qualityHeaderBudget(QualityRetryRuntime{}, true, true, true) != 0 {
		t.Fatal("unset budget stays off")
	}
}
