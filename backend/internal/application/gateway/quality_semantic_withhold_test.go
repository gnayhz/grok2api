package gateway

import (
	"context"
	"strings"
	"io"
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
