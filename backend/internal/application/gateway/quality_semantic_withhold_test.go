package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// 终态"纯语义输出"（仅工具调用、零思考零文本）在思考期望内按
// missing-thinking 扣留——降智账号的裸工具调用此前经语义放行出口
// 整包交付且守卫零计数（terminal_burst 形态：恒定小输出、7 事件、
// 首字==耗时、usage reasoning=0）。effort=none（未期望思考）与零值
// 调用方（探针）保持放行。

// responses 裸 function_call 流（无 reasoning item、无文本）。
func TestSemanticOnlyToolCallWithholdsWhenThinkingExpected(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, ReasoningExpected: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":130,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	if peekErr != nil {
		t.Fatalf("peek err=%v", peekErr)
	}
	if verdict != QualityWithhold {
		t.Fatalf("思考期望内的纯工具调用流应扣留：verdict=%s", verdict)
	}
}

// effort=none / 零值调用方：语义放行保持（合法零思考形态）。
func TestSemanticOnlyToolCallDeliversWhenThinkingNotExpected(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\"}\"}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	if peekErr != nil {
		t.Fatalf("peek err=%v", peekErr)
	}
	if verdict != QualityDeliver {
		t.Fatalf("未期望思考的纯工具调用流应放行：verdict=%s", verdict)
	}
}

// chat 协议纯 tool_calls 流同理扣留；非流式 body 路径同语义。
func TestSemanticOnlyChatToolCallsWithholdAndBodyPath(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, ReasoningExpected: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"read\"}}]}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolChat, cfg)
	_ = reader.Close()
	if peekErr != nil || verdict != QualityWithhold {
		t.Fatalf("chat 纯 tool_calls 流应扣留：verdict=%s err=%v", verdict, peekErr)
	}
	body := `{"choices":[{"message":{"tool_calls":[{"id":"call-1","function":{"name":"read","arguments":"{}"}}]}}]}`
	_, bodyVerdict, _, _ := peekQualityBody(io.NopCloser(strings.NewReader(body)), cfg)
	if bodyVerdict != QualityWithhold {
		t.Fatalf("非流式纯 tool_calls body 应扣留：verdict=%s", bodyVerdict)
	}
}

// web_search_call item 头只抬 semanticOutput（证据截止不误杀搜索静默），
// 不得被当成健康证据。思考期望内终态仍扣留；classifyQualityHold 在
// 仅有该头时保持 Wait。
func TestWebSearchCallHeaderIsLivenessNotHealth(t *testing.T) {
	t.Parallel()
	mid := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&mid, []byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"ws_1\",\"type\":\"web_search_call\"}}\n\n"))
	if !mid.semanticOutput {
		t.Fatal("web_search_call item header must mark semanticOutput")
	}
	if v := classifyQualityHold(mid.signals()); v != QualityWait {
		t.Fatalf("search item header must not deliver: verdict=%s", v)
	}

	cfg := QualityRetryRuntime{Enabled: true, ReasoningExpected: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r_ws\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"ws_1\",\"type\":\"web_search_call\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ws_1\",\"type\":\"web_search_call\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r_ws\",\"status\":\"completed\"}}\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	if peekErr != nil {
		t.Fatalf("peek err=%v", peekErr)
	}
	if verdict != QualityWithhold {
		t.Fatalf("thinking-expected web_search_call-only stream must withhold: verdict=%s", verdict)
	}

	open := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader2, writer2 := io.Pipe()
	go func() {
		_, _ = writer2.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r_ws\"}}\n\n"))
		_, _ = writer2.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"ws_1\",\"type\":\"web_search_call\"}}\n\n"))
		_, _ = writer2.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
		_ = writer2.Close()
	}()
	_, openVerdict, _, openErr := peekQualityStream(context.Background(), reader2, qualityProtocolResponses, open)
	_ = reader2.Close()
	if openErr != nil || openVerdict != QualityDeliver {
		t.Fatalf("thinking-not-expected search-only stream must deliver: verdict=%s err=%v", openVerdict, openErr)
	}
}

// reasoning item.added 开思考项后插入 web_search_call：转换器会推迟搜索 UI，
// 扫描器仍只抬 semanticOutput。不得当思考证据；推理闭合无增量仍扣留。
func TestReasoningHeaderPlusWebSearchIsNotThinkingEvidence(t *testing.T) {
	t.Parallel()
	mid := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&mid, []byte(strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"ws_1","type":"web_search_call"}}`,
		"",
	}, "\n\n")))
	if mid.hasThinking {
		t.Fatal("web_search_call during reasoning header must not be thinking evidence")
	}
	if !mid.semanticOutput {
		t.Fatal("web_search_call must still mark liveness")
	}
	if v := classifyQualityHold(mid.signals()); v != QualityWait {
		t.Fatalf("mid-search during empty reasoning must wait, not deliver: %s", v)
	}

	cfg := QualityRetryRuntime{Enabled: true, ReasoningExpected: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"ws_1","type":"web_search_call"}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"sig"}}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
		"",
	}, "\n\n")
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolResponses, cfg)
	if replay != nil {
		_ = replay.Close()
	}
	if err != nil || verdict != QualityWithhold {
		t.Fatalf("reasoning closed without thinking + search = %s err=%v, want withhold", verdict, err)
	}
}

// 思考期望解析：none 之外一律期望（含空档）。
func TestReasoningExpectedForEffort(t *testing.T) {
	t.Parallel()
	for _, effort := range []string{"", "auto", "low", "medium", "high", "xhigh", "fixed", "XHIGH"} {
		if !reasoningExpectedForEffort(effort) {
			t.Fatalf("effort=%q 应视为期望思考", effort)
		}
	}
	for _, effort := range []string{"none", "NONE", " none "} {
		if reasoningExpectedForEffort(effort) {
			t.Fatalf("effort=%q 应视为不期望思考", effort)
		}
	}
}
