package gateway

// 密文证伪锁定（测试环境 A/B 实测抓流）：
//   - RSC risk 降智账号（3025）：零 reasoning_summary_text.delta，仅
//     output_item.done 携带 4167B encrypted_content + 正常文本输出。
//   - clean 账号（3022，6/6 条流）：42-83 条 reasoning_summary_text.delta
//     + 密文 + 文本。
// 结论：encrypted_content / signature_delta / reasoning-start 注释在降智流
// 与健康流中都存在，毫无判别力；只有可见思考增量是健康证据。此前的
// "密文=证据" 吸收回滚，以下测试锁定回滚后的正确行为。

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// 降智流原始形态（3025 抓流脱敏）：密文齐全、零可见思考、有可见输出。
func TestDegradedCiphertextOnlyStreamIsWithheld(t *testing.T) {
	t.Parallel()
	ciphertext := "gAAAAAB1" + strings.Repeat("x", 4159)
	content := strings.Repeat("word ", 40)
	state := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&state, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+ciphertext+`"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"msg_1","type":"message"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+content+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"`+ciphertext+`"},{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"output_tokens":95,"output_tokens_details":{"reasoning_tokens":40}}}}`,
	)))
	sig := state.signals()
	if sig.HasThinking {
		t.Fatalf("密文不得构成思考证据: %#v", sig)
	}
	if v := classifyQualityHold(sig); v != QualityWithhold {
		t.Fatalf("降智流（仅密文+可见输出）= %s，应扣留", v)
	}
}

// 中途即拦截：reasoning item 打开 + 可见输出达到阈值，不等终态密文。
func TestDegradedStreamWithheldMidStreamBeforeCiphertext(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 40)
	state := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&state, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+content+`"}`,
	)))
	sig := state.signals()
	if v := classifyQualityHold(sig); v != QualityWithhold {
		t.Fatalf("中途降智流 = %s，应尽早扣留（不等待流末密文）", v)
	}
}

// 健康流原始形态（3022 抓流）：可见思考增量在前，密文在后。
func TestCleanVisibleSummaryDeltaStreamDelivers(t *testing.T) {
	t.Parallel()
	ciphertext := "gAAAAAB1" + strings.Repeat("x", 4159)
	content := strings.Repeat("word ", 40)
	state := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&state, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"先分析袋中球数"}`,
		`data: {"type":"response.reasoning_summary_part.done"}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+ciphertext+`"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+content+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":95,"output_tokens_details":{"reasoning_tokens":40}}}}`,
	)))
	sig := state.signals()
	if !sig.HasThinking {
		t.Fatalf("可见思考增量必须是证据: %#v", sig)
	}
	if v := classifyQualityHold(sig); v != QualityDeliver {
		t.Fatalf("健康流 = %s，应放行", v)
	}
}

// Messages 协议：signature_delta（密文的协议内表达）不是证据。
func TestAnthropicSignatureDeltaIsNotEvidence(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 40)
	state := qualityScanState{protocol: qualityProtocolAnthropic}
	observeQualityChunk(&state, []byte(sse(
		`data: {"type":"content_block_start","content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"signature_delta","signature":"EqoBCkgIYj"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"`+content+`"}}`,
		`data: {"type":"message_stop"}`,
	)))
	sig := state.signals()
	if sig.HasThinking {
		t.Fatalf("signature_delta 不得构成思考证据: %#v", sig)
	}
	if v := classifyQualityHold(sig); v != QualityWithhold {
		t.Fatalf("仅签名的降智流 = %s，应扣留", v)
	}
}

// chat 转换的 reasoning-start 注释不是证据。
func TestChatReasoningStartCommentIsNotEvidence(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 40)
	state := qualityScanState{protocol: qualityProtocolChat}
	observeQualityChunk(&state, []byte(sse(
		": grok2api-reasoning-start",
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
	)))
	sig := state.signals()
	if sig.HasThinking {
		t.Fatalf("reasoning-start 注释不得构成思考证据: %#v", sig)
	}
	if v := classifyQualityHold(sig); v != QualityWithhold {
		t.Fatalf("注释+输出的降智流 = %s，应扣留", v)
	}
}

func TestIdleAccountCooldownNormalizesAndStreams(t *testing.T) {
	t.Parallel()
	if got := normalizeQualityRetry(QualityRetryRuntime{}).IdleAccountCooldown; got != qualityIdleAccountCooldown {
		t.Fatalf("default idle cooldown = %s, want %s", got, qualityIdleAccountCooldown)
	}
	if got := normalizeQualityRetry(QualityRetryRuntime{IdleAccountCooldown: time.Minute}).IdleAccountCooldown; got != time.Minute {
		t.Fatalf("explicit idle cooldown not preserved: %s", got)
	}
}

// Messages 未请求 thinking（修正）：流式照常 hold——转换器以
// ThinkingEvidenceComment 内部注释保留思考证据（上游对未指定强度的请求
// 按默认强度思考，零思考即降智；原整体豁免放行了 15 条零思考交付）。
// 非流式 body 无注释通道，保留豁免（已知残留缺口，REASONING0_LEDGER §C2）。
func TestMessagesWithoutThinkingHoldPolicy(t *testing.T) {
	t.Parallel()
	route := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.6"}
	cfg := QualityRetryRuntime{Enabled: true}
	gate := func(body string, streaming bool) bool {
		return shouldHoldQualityStream(Input{Streaming: streaming, Body: []byte(body), PublicModel: "grok-4.6"}, nil, route, audit.OperationMessages, cfg)
	}
	for _, tc := range []struct{ name, body string }{
		{name: "no thinking field", body: `{"model":"grok-4.6","max_tokens":800,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "thinking disabled", body: `{"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`},
		{name: "thinking zero budget", body: `{"thinking":{"type":"enabled","budget_tokens":0},"messages":[{"role":"user","content":"hi"}]}`},
	} {
		streamingHold := gate(tc.body, true)
		if tc.name == "no thinking field" {
			// 未指定 thinking：流式照常 hold（证据注释通道已补），
			// 非流式无注释通道保留豁免。
			if !streamingHold {
				t.Errorf("[%s] 流式 messages 未请求 thinking 应照常 hold: %s", tc.name, tc.body)
			}
			if gate(tc.body, false) {
				t.Errorf("[%s] 非流式无证据通道保留豁免: %s", tc.name, tc.body)
			}
			continue
		}
		// 显式关闭思考（disabled/零预算）不再豁免（删除
		// reasoning_disabled）：白名单内模型（grok-4.5/4.6）不支持 none，
		// 显式关闭是非法组合——流式照常进守卫（上游将以 400 拒绝，判决
		// 无从发生）；非流式 messages 仍由 messages_thinking_off 豁免。
		if !streamingHold {
			t.Errorf("[%s] 显式关思考在 none-不支持模型上应照常进守卫: %s", tc.name, tc.body)
		}
	}
	if !gate(`{"thinking":{"type":"enabled","budget_tokens":1500},"messages":[{"role":"user","content":"hi"}]}`, true) {
		t.Error("messages 显式 thinking 应照常 hold")
	}
	// 其余协议不受该豁免影响。
	if !shouldHoldQualityStream(Input{Streaming: true, Body: []byte(`{"input":"hi"}`), PublicModel: "grok-4.6"}, nil, route, audit.OperationResponses, cfg) {
		t.Error("responses 请求应照常 hold")
	}
}

// 非流式 body 判决锁定（fixture 取自 阶段 A 非流式实测形态：
// clean 4/4 summary 97-101 字，risk 7/7 summary=0 且密文照常存在）。
func TestPeekQualityBodyClassifiesRealShapes(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true}
	content := strings.Repeat("word ", 40)

	// 降智形态：仅密文 reasoning + 可见文本，无 summary → withhold。
	degraded := `{"id":"resp_d","output":[{"type":"reasoning","encrypted_content":"gAAAAAB1x","summary":[]},{"type":"message","content":[{"type":"output_text","text":"` + content + `"}]}],"usage":{"output_tokens":95,"output_tokens_details":{"reasoning_tokens":40}}}`
	replay, verdict, usage, err := peekQualityBody(io.NopCloser(strings.NewReader(degraded)), cfg)
	if err != nil || verdict != QualityWithhold {
		t.Fatalf("降智 body = %s err=%v，应扣留", verdict, err)
	}
	if usage.OutputTokens != 95 || usage.ReasoningTokens != 40 {
		t.Fatalf("usage 未解析: %#v", usage)
	}
	replayed, _ := io.ReadAll(replay)
	if string(replayed) != degraded {
		t.Fatal("body 重放不完整（下游转换会拿到损坏数据）")
	}

	// 健康形态：summary 可见思考文本 → deliver。
	clean := `{"id":"resp_c","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"先分析袋中球数"}]},{"type":"message","content":[{"type":"output_text","text":"` + content + `"}]}],"usage":{"output_tokens":95}}`
	_, verdict, _, err = peekQualityBody(io.NopCloser(strings.NewReader(clean)), cfg)
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("健康 body = %s err=%v，应放行", verdict, err)
	}

	// 纯工具调用输出：语义输出 → deliver（与流式 R5 同语义）。
	tools := `{"id":"resp_t","output":[{"type":"function_call","call_id":"c1","name":"read"}]}`
	_, verdict, _, err = peekQualityBody(io.NopCloser(strings.NewReader(tools)), cfg)
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("工具调用 body = %s err=%v，应放行", verdict, err)
	}

	// 空响应（output 存在但零内容零思考）→ 空流错误（可重试路径）。
	empty := `{"id":"resp_e","output":[]}`
	_, verdict, _, err = peekQualityBody(io.NopCloser(strings.NewReader(empty)), cfg)
	if verdict != QualityWait || err == nil || !errors.Is(err, errQualityEmptyStream) {
		t.Fatalf("空 body = %s err=%v，应为空流", verdict, err)
	}

	// 非法 JSON → 空流（不猜质量）：verdict 同为 Wait（可重试路径）。
	_, verdict, _, err = peekQualityBody(io.NopCloser(strings.NewReader("not-json")), cfg)
	if verdict != QualityWait || err == nil || !errors.Is(err, errQualityEmptyStream) {
		t.Fatalf("非法 JSON 应按空流处理，verdict=%s err=%v", verdict, err)
	}

	// 未识别形状（既非 Responses 也非转换后的 chat/messages 形态）→ fail-open
	// 放行。注：chat 形态自 round 41 起会被识别并判决，不再是 fail-open。
	alien := `{"result":"ok","status":"fine"}`
	_, verdict, _, err = peekQualityBody(io.NopCloser(strings.NewReader(alien)), cfg)
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("未识别形状应 fail-open，verdict=%s err=%v", verdict, err)
	}

	// 门控：非流式 responses 请求现在也进入 hold。
	route := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.6"}
	if !shouldHoldQualityStream(Input{Streaming: false, Body: []byte(`{"input":"hi"}`), PublicModel: "grok-4.6"}, nil, route, audit.OperationResponses, cfg) {
		t.Error("非流式 responses 请求应进入 hold（此前的豁免导致降智 body 直接交付）")
	}
}

// 零证据截止锁定（实测：降智静默期 75-121s，干净证据 2.1s）。
func TestPeekEvidenceTimeoutBoundsSilentDegradedStream(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 150 * time.Millisecond}
	reader, writer := io.Pipe()
	go func() {
		time.Sleep(600 * time.Millisecond) // 静默期远超截止
		_, _ = writer.Write([]byte(`data: {"type":"response.output_text.delta","delta":"word word word word"}` + "\n\n"))
		_ = writer.Close()
	}()
	started := time.Now()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	elapsed := time.Since(started)
	if !errors.Is(peekErr, errQualityEvidenceTimeout) {
		t.Fatalf("静默流应在截止时中止：verdict=%s err=%v", verdict, peekErr)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("截止应先于迟到输出触发：elapsed=%s", elapsed)
	}
}

func TestPeekEvidenceTimeoutDeliversWhenEvidenceArrives(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 400 * time.Millisecond}
	content := strings.Repeat("word ", 40)
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"先想一步"}` + "\n\n"))
		_, _ = writer.Write([]byte(`data: {"type":"response.output_text.delta","delta":"` + content + `"}` + "\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	if peekErr != nil || verdict != QualityDeliver {
		t.Fatalf("截止内的证据流应放行：verdict=%s err=%v", verdict, peekErr)
	}
}

func TestPeekEvidenceTimeoutDefaultNormalized(t *testing.T) {
	t.Parallel()
	if got := normalizeQualityRetry(QualityRetryRuntime{}).EvidenceTimeout; got != defaultQualityEvidenceTimeout {
		t.Fatalf("default evidence timeout = %s, want %s", got, defaultQualityEvidenceTimeout)
	}
	if got := normalizeQualityRetry(QualityRetryRuntime{EvidenceTimeout: time.Second}).EvidenceTimeout; got != time.Second {
		t.Fatalf("explicit evidence timeout not preserved: %s", got)
	}
}

// 首事件截止锁定（直连复测：降智排队期间零 data 事件 68-125s，
// clean 首事件 0.8-2.2s 恒定；keepalive 注释不算 data 事件）。
func TestPeekCreatedTimeoutAbortsZeroEventStream(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, CreatedTimeout: 120 * time.Millisecond, EvidenceTimeout: 400 * time.Millisecond}
	reader, writer := io.Pipe()
	go func() {
		// 排队形态：仅 keepalive 注释 + 迟到的 created（远超首事件截止）
		time.Sleep(60 * time.Millisecond)
		_, _ = writer.Write([]byte(": keepalive\n\n"))
		time.Sleep(300 * time.Millisecond)
		_, _ = writer.Write([]byte(`data: {"type":"response.created","response":{"id":"r1"}}` + "\n\n"))
		_ = writer.Close()
	}()
	started := time.Now()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	if !errors.Is(peekErr, errQualityCreatedTimeout) {
		t.Fatalf("零 data 事件流应在首事件截止中止：verdict=%s err=%v", verdict, peekErr)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("首事件截止应先于迟到 created 触发：%s", elapsed)
	}
}

func TestPeekCreatedTimeoutInactiveAfterFirstDataEvent(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, CreatedTimeout: 100 * time.Millisecond, EvidenceTimeout: 900 * time.Millisecond}
	content := strings.Repeat("word ", 40)
	reader, writer := io.Pipe()
	go func() {
		// 数据事件按时到达（60ms < 100ms 截止），之后静默——由证据截止接管
		time.Sleep(60 * time.Millisecond)
		_, _ = writer.Write([]byte(`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}` + "\n\n"))
		time.Sleep(500 * time.Millisecond)
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	// 首 chunk 后流关闭且无证据无输出 → 终态空流路径，而非首事件截止
	if errors.Is(peekErr, errQualityCreatedTimeout) {
		t.Fatalf("数据事件已到达，首事件截止不得再触发：%v", peekErr)
	}
	if verdict != QualityWait {
		t.Fatalf("空流应为 Wait，got %s", verdict)
	}
	_ = content
}

func TestPeekCreatedTimeoutDefaultNormalized(t *testing.T) {
	t.Parallel()
	if got := normalizeQualityRetry(QualityRetryRuntime{}).CreatedTimeout; got != defaultQualityCreatedTimeout {
		t.Fatalf("default created timeout = %s, want %s", got, defaultQualityCreatedTimeout)
	}
	if got := normalizeQualityRetry(QualityRetryRuntime{CreatedTimeout: 2 * time.Second}).CreatedTimeout; got != 2*time.Second {
		t.Fatalf("explicit created timeout not preserved: %s", got)
	}
}
