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
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("early abort must decide at the 30ms budget, not wait out the 200ms header delay: %s", elapsed)
	}
	fixture.assertAttempts(t, 0, 1)
	fixture.assertAttempts(t, 1, 1)
}

// TestEarlyHeaderAbortFiresOncePerRequest：第一次触发后预算解除——第二次
// 尝试即使头迟滞也应完整等待并按流内容判定（避免系统性慢路径 fail-closed）。
func TestEarlyHeaderAbortFiresOncePerRequest(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	// 两个账号都头迟滞但流内容健康：第一次触发预算中止，第二次必须等下去。
	for index := range fixture.credentials {
		fixture.adapter.responses[fixture.credentials[index].ID] = []scriptedBuildResponse{
			{status: 200, body: aFormStream(), headerDelay: 120 * time.Millisecond},
		}
	}

	cfg := baseSameAccountRuntime()
	cfg.EarlyHeaderAbort = 30 * time.Millisecond
	service := fixture.service(t, cfg)

	result, err := service.CreateChatCompletion(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("second attempt must wait past the disarmed budget, err=%v", err)
	}
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !strings.Contains(string(body), "reasoning_content") {
		t.Fatal("slow-but-healthy stream must be delivered, not aborted")
	}
	// Prove the budget actually fired: account 0 was aborted at the header
	// stage and the request switched to account 1, which then waited past
	// the disarmed budget. Without the abort, account 0 would deliver and
	// account 1 would never be attempted (vacuous-test fix).
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

// TestEarlyHeaderBudgetHelper：预算只在 hold 激活、已配置、单发未用时生效。
func TestEarlyHeaderBudgetHelper(t *testing.T) {
	cfg := QualityRetryRuntime{EarlyHeaderAbort: 10 * time.Second}
	if qualityHeaderBudget(cfg, true, true) != 10*time.Second {
		t.Fatal("armed+enabled must return the budget")
	}
	if qualityHeaderBudget(cfg, false, true) != 0 {
		t.Fatal("hold disabled must disarm")
	}
	if qualityHeaderBudget(cfg, true, false) != 0 {
		t.Fatal("already fired must disarm")
	}
	if qualityHeaderBudget(QualityRetryRuntime{}, true, true) != 0 {
		t.Fatal("unset budget stays off")
	}
}
