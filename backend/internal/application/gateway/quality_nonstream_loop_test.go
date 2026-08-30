package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

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
		RequestID: "req-nonstream-hold", ClientKey: clientkey.Key{ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
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
	result.Finalize(Usage{}, "nonstream-ok", "")
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
		RequestID: "req-nonstream-fc", ClientKey: clientkey.Key{ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
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
		RequestID: "req-nonstream-msg", ClientKey: clientkey.Key{ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
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
	result.Finalize(Usage{}, "msg-nonstream-ok", "")
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
		RequestID: "req-msg-fc", ClientKey: clientkey.Key{ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: false,
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
