package gateway

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	audit "github.com/chenyme/grok2api/backend/internal/domain/audit"
	clientkey "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// --- hold ---
// TestAttemptLoopNonStreamChatHold：非流式 chat 请求的 body 已是转换后的
// 客户端形态（round 41 起被识别判决）。端到端锁定完整循环：降智 chat
// JSON（零思考有正文）扣留换号，健康 chat JSON（带 reasoning_content）
// 交付。
func TestAttemptLoopNonStreamChatHold(t *testing.T) {
	ctx := context.Background()
	degradedChat := `{"id":"chatcmpl-deg","model":"grok-4.6","choices":[{"message":{"content":"no thinking anywhere in this non-stream answer"}}]}`
	healthyChat := `{"id":"chatcmpl-ok","model":"grok-4.6","usage":{"prompt_tokens":5,"completion_tokens":30,"completion_tokens_details":{"reasoning_tokens":18}},"choices":[{"message":{"reasoning_content":"think it through","content":"a considered answer"}}]}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, credentials := newGuardLoopService(t, adapter, "nonstream-degraded", "nonstream-healthy")
	adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degradedChat}}
	adapter.responses[credentials[1].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: healthyChat}}

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-nonstream-hold", ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"write a game"}]}`),
	})
	if err != nil {
		t.Fatalf("non-stream chat should deliver after withhold retry, err=%v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	body, _ := io.ReadAll(result.Body)
	if !strings.Contains(string(body), "a considered answer") {
		t.Fatalf("delivered body = %s", body)
	}
	finishTestResult(t, result, Usage{}, "nonstream-ok", "")
	_ = result.Body.Close()
	attempts := adapter.Attempts()
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v, want degraded-then-healthy across accounts", attempts)
	}
}

// --- failclosed ---
// TestAttemptLoopNonStreamFailClosed：非流式降智 body 在预算耗尽时同样
// Fail-Closed——降智字节一个都不能到客户端（流式路径的镜像锁定）。两账号
// 都返回零思考 chat JSON，fail_closed 下必须 503 且正文不含降智内容。
func TestAttemptLoopNonStreamFailClosed(t *testing.T) {
	ctx := context.Background()
	degraded := `{"id":"chatcmpl-leak","choices":[{"message":{"content":"SECRET_DEGRADED_PAYLOAD"}}]}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, credentials := newGuardLoopService(t, adapter, "nonstream-fc-one", "nonstream-fc-two")
	adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degraded}}
	adapter.responses[credentials[1].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degraded}}

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-nonstream-fc", ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"anything"}]}`),
	})
	if err == nil && result.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(result.Body)
		_ = result.Body.Close()
		t.Fatalf("exhausted non-stream must fail closed, status=%d body=%s", result.StatusCode, body)
	}
	if err == nil {
		_ = result.Body.Close()
	}
	var failure *UpstreamFailure
	if !errors.As(err, &failure) || failure.Code != ErrorQualityDegraded {
		t.Fatalf("err = %v, want quality degraded upstream failure", err)
	}
	if strings.Contains(failure.PublicMessage, "SECRET_DEGRADED_PAYLOAD") {
		t.Fatal("degraded bytes must never leak into the failure surface")
	}
	if attempts := adapter.Attempts(); len(attempts) != 2 {
		t.Fatalf("attempts = %v, want both accounts tried", attempts)
	}
}

// --- messages ---
// TestAttemptLoopNonStreamMessagesHold：非流式 Messages(anthropic) 请求的 body 已是转换后的
// 客户端形态（round 41 起被识别判决）。round 43 chat e2e 的镜像：降智
// messages body（纯 text 块）扣留换号，健康 body（thinking 块 + text）交付。

func TestAttemptLoopNonStreamMessagesHold(t *testing.T) {
	ctx := context.Background()
	degradedBody := `{"id":"msg-deg","content":[{"type":"text","text":"bare anthropic answer with no thinking"}]}`
	healthyBody := `{"id":"msg-ok","content":[{"type":"thinking","thinking":"reason it out"},{"type":"text","text":"a considered anthropic answer"}]}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, credentials := newGuardLoopService(t, adapter, "msg-nonstream-degraded", "msg-nonstream-healthy")
	adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degradedBody}}
	adapter.responses[credentials[1].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: healthyBody}}

	result, err := service.CreateMessage(ctx, Input{
		RequestID: "req-nonstream-msg", ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hello"}],"thinking":{"type":"enabled","budget_tokens":1024}}`),
	})
	if err != nil {
		t.Fatalf("non-stream messages should deliver after withhold retry, err=%v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	body, _ := io.ReadAll(result.Body)
	if !strings.Contains(string(body), "a considered anthropic answer") {
		t.Fatalf("delivered body = %s", body)
	}
	finishTestResult(t, result, Usage{}, "msg-nonstream-ok", "")
	_ = result.Body.Close()
	attempts := adapter.Attempts()
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v, want degraded-then-healthy across accounts", attempts)
	}
}

// --- messages_fc ---
// TestAttemptLoopNonStreamMessagesFailClosed：非流式降智 body 在预算耗尽时同样
// Fail-Closed——降智字节一个都不能到客户端（流式路径的镜像锁定）。两账号
// 都返回零思考 chat JSON，fail_closed 下必须 503 且正文不含降智内容。
func TestAttemptLoopNonStreamMessagesFailClosed(t *testing.T) {
	ctx := context.Background()
	degraded := `{"content":[{"type":"text","text":"SECRET_DEGRADED_PAYLOAD"}]}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, credentials := newGuardLoopService(t, adapter, "msg-fc-one", "msg-fc-two")
	adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degraded}}
	adapter.responses[credentials[1].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degraded}}

	result, err := service.CreateMessage(ctx, Input{
		RequestID: "req-msg-fc", ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"anything"}],"thinking":{"type":"enabled","budget_tokens":1024}}`),
	})
	if err == nil && result.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(result.Body)
		_ = result.Body.Close()
		t.Fatalf("exhausted non-stream messages must fail closed, status=%d body=%s", result.StatusCode, body)
	}
	if err == nil {
		_ = result.Body.Close()
	}
	var failure *UpstreamFailure
	if !errors.As(err, &failure) || failure.Code != ErrorQualityDegraded {
		t.Fatalf("err = %v, want quality degraded upstream failure", err)
	}
	if strings.Contains(failure.PublicMessage, "SECRET_DEGRADED_PAYLOAD") {
		t.Fatal("degraded bytes must never leak into the failure surface")
	}
	if attempts := adapter.Attempts(); len(attempts) != 2 {
		t.Fatalf("attempts = %v, want both accounts tried", attempts)
	}
}

// TestAttemptLoopFailClosedAuditCarriesRule 锚定审计归因:
// 耗尽拒绝的 503 主行必须带最终判决规则指纰
// (此前仅交付尝试记规则,拒绝行的 quality_rule
// 恒空——面板对 503 归因必须逐条展开 attempt 明细)。
func TestAttemptLoopFailClosedAuditCarriesRule(t *testing.T) {
	ctx := context.Background()
	degraded := `{"id":"chatcmpl-rule","choices":[{"message":{"content":"SECRET_DEGRADED_PAYLOAD"}}]}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, credentials, database, _ := newGuardLoopServiceWithDB(t, adapter, "nonstream-rule-one", "nonstream-rule-two")
	adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degraded}}
	adapter.responses[credentials[1].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: degraded}}

	_, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-nonstream-rule", ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"anything"}]}`),
	})
	var failure *UpstreamFailure
	if !errors.As(err, &failure) || failure.Code != ErrorQualityDegraded {
		t.Fatalf("err = %v, want quality degraded", err)
	}
	records, total, listErr := database.List(ctx, 0, 20)
	if listErr != nil {
		t.Fatalf("查询审计主行: %v", listErr)
	}
	var found *audit.Record
	for i := range records {
		if records[i].RequestID == "req-nonstream-rule" {
			found = &records[i]
		}
	}
	if found == nil {
		t.Fatalf("审计主行必须落库 (total=%d)", total)
	}
	if found.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("主行状态应 503, got %d", found.StatusCode)
	}
	if found.QualityRule == "" {
		t.Fatal("耗尽拒绝的审计主行必须带判决规则指纹(quality_rule)")
	}
}

// TestAttemptLoopConcurrentLeasesReleased 锚定并发租约不泄漏:
// MaxConcurrent=1 的两账号在 8 路并发下至多 2 路拿到
// 租约、其余得并发上限拒绝;全部落定后再发
// 两路必须成功——失败路径的 release 若泄漏,
// 后续请求会被残留租约永久拒绝(批10
// 并发压测的回归锈定)。
func TestAttemptLoopConcurrentLeasesReleased(t *testing.T) {
	ctx := context.Background()
	healthy := `{"id":"chatcmpl-ok","usage":{"prompt_tokens":5,"completion_tokens":30,"completion_tokens_details":{"reasoning_tokens":18}},"choices":[{"message":{"reasoning_content":"think","content":"answer"}}]}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, credentials := newGuardLoopService(t, adapter, "conc-one", "conc-two")
	for _, credential := range credentials {
		adapter.responses[credential.ID] = []scriptedBuildResponse{{status: http.StatusOK, body: healthy}, {status: http.StatusOK, body: healthy}, {status: http.StatusOK, body: healthy}}
	}

	const burst = 8
	var wg sync.WaitGroup
	succeeded, limited, failed := 0, 0, 0
	var mu sync.Mutex
	results := make([]*Result, 0, burst)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			result, err := service.CreateChatCompletion(ctx, Input{
				RequestID: fmt.Sprintf("req-conc-%d", n), ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
				Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}]}`),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
				results = append(results, result)
			default:
				var selection *selector.SelectionUnavailableError
				if errors.As(err, &selection) {
					limited++
				} else {
					failed++
					t.Logf("UNEXPECT %T: %v", err, err)
				}
			}
		}(i)
	}
	wg.Wait()
	if failed != 0 {
		t.Fatalf("并发路径不得出现非预期失败, failed=%d", failed)
	}
	if succeeded > 2 {
		t.Fatalf("MaxConcurrent=1 ×2 账号下至多 2 路拿到租约, succeeded=%d", succeeded)
	}
	// 交付路径的租约随 Finalize/Close 释放(与真实
	// HTTP 生命周期同款)——先收尾再验证。
	for _, result := range results {
		finishTestResult(t, result, Usage{}, "conc-done", "")
		_ = result.Body.Close()
	}
	// 全部落定后租约必须已释放:后续两路串行成功。
	// 后续结果同样必须终结——不终结则请求 0
	// 永久持租约(与生产 transport 层恒定
	// 终结的语义不同),且测试内 sticky 亲缘
	// 可能把下一路钉到同一被占账号——
	// 这是第二循环抓到的偶发失败(≈1/10)根因。
	for i := 0; i < 2; i++ {
		result, err := service.CreateChatCompletion(ctx, Input{
			RequestID: fmt.Sprintf("req-conc-after-%d", i), ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
			Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}]}`),
		})
		if err != nil {
			t.Fatalf("并发落定后租约泄漏(后续请求 %d 失败): %v", i, err)
		}
		finishTestResult(t, result, Usage{}, fmt.Sprintf("conc-after-%d", i), "")
		_ = result.Body.Close()
	}
}
