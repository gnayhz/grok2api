package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// 空白思考增量不构成思考证据（线上事故形态锁定）。
//
// 生产抓流实证：健康 summary 尾部会补发一个只含换行的
// reasoning_summary_text.delta；降智流的空推理项在末尾整包 flush 时
// 同样可能只携带这类空白增量。旧判定仅检查 Delta != ""，空白增量即
// hasThinking → 规则 1 瞬间放行整包降智响应（审计形态：terminal_burst
// / usage reasoning=0 / 单次尝试 200 交付 / 守卫零计数——Deliver 判决
// 不进任何统计）。空白既不含可见思考内容，也不产生 reasoning token，
// 不得作为证据；带内容的增量（即使首尾有空白）仍是即时证据。

// 事故原形：降智末尾整包 flush——空白 summary 增量 + rs 项闭合 + 整段
// 答案单增量。修复前规则 1 在空白增量处放行；修复后 rs 项闭合零思考
// 由规则 2 扣留（正文增量到达则规则 3 同样扣留）。
func TestBlankSummaryDeltaDoesNotDeliverTerminalBurstFlush(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"\\n\"}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("answer ", 20) + "\"}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":140,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	if peekErr != nil {
		t.Fatalf("peek err=%v", peekErr)
	}
	if verdict != QualityWithhold {
		t.Fatalf("空白 summary 增量不得放行降智整包 flush：verdict=%s（应 Withhold）", verdict)
	}
}

// chat 协议同型：reasoning_content 空白增量 + 正文，应按规则 3 扣留。
func TestBlankChatReasoningDeltaDoesNotDeliver(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\" \\n\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("word ", 40) + "\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolChat, cfg)
	_ = reader.Close()
	if peekErr != nil {
		t.Fatalf("peek err=%v", peekErr)
	}
	if verdict != QualityWithhold {
		t.Fatalf("空白 reasoning_content 不得放行零思考正文：verdict=%s（应 Withhold）", verdict)
	}
}

// Messages 协议同型：thinking_delta 空白增量后 thinking 块闭合，应按
// 规则 2（Messages 形态）扣留。
func TestBlankAnthropicThinkingDeltaDoesNotDeliver(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"thinking\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"\\t\"}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"content_block_stop\",\"content_block\":{}}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolAnthropic, cfg)
	_ = reader.Close()
	if peekErr != nil {
		t.Fatalf("peek err=%v", peekErr)
	}
	if verdict != QualityWithhold {
		t.Fatalf("空白 thinking_delta 不得放行零思考正文：verdict=%s（应 Withhold）", verdict)
	}
}

// 首尾带空白但含内容的增量仍是即时证据（防过度收紧）。
func TestWhitespacePaddedEvidenceStillDelivers(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"\\n先想一步\\n\"}\n\n"))
		_, _ = writer.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	if peekErr != nil {
		t.Fatalf("peek err=%v", peekErr)
	}
	if verdict != QualityDeliver {
		t.Fatalf("含内容的增量必须保持即时放行：verdict=%s", verdict)
	}
}

// 单元级锁定：三协议的空白增量形态一律不置 hasThinking。
func TestBlankThinkingDeltaNeverSetsHasThinking(t *testing.T) {
	t.Parallel()
	responses := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&responses, []byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"\\r\\n\"}\n\n"))
	observeQualityChunk(&responses, []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\" \\t \"}\n\n"))
	if responses.hasThinking {
		t.Fatal("responses 空白增量不得置 hasThinking")
	}
	observeQualityChunk(&responses, []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"x\"}\n\n"))
	if !responses.hasThinking {
		t.Fatal("含内容增量必须置 hasThinking")
	}

	chat := qualityScanState{protocol: qualityProtocolChat}
	observeQualityChunk(&chat, []byte("data: {\"choices\":[{\"delta\":{\"reasoning\":\"\\n\"}}]}\n\n"))
	observeQualityChunk(&chat, []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"\"}}]}\n\n"))
	observeQualityChunk(&chat, []byte("data: {\"choices\":[{\"delta\":{\"thinking_content\":\"   \"}}]}\n\n"))
	if chat.hasThinking {
		t.Fatal("chat 空白增量不得置 hasThinking")
	}

	anthropic := qualityScanState{protocol: qualityProtocolAnthropic}
	observeQualityChunk(&anthropic, []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"\\n\"}}\n\n"))
	if anthropic.hasThinking {
		t.Fatal("anthropic 空白增量不得置 hasThinking")
	}
	observeQualityChunk(&anthropic, []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"思考\"}}\n\n"))
	if !anthropic.hasThinking {
		t.Fatal("anthropic 含内容增量必须置 hasThinking")
	}
}
