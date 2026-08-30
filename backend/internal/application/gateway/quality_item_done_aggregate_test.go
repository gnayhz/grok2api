package gateway

import (
	"io"
	"strings"
	"testing"
)

// TestItemDoneAggregateMessageNotMisclassified：message 全文仅经
// output_item.done 送达（无增量、completed 帧也不重放 output）的流形态。
// 修复前该形态零可见计数，终态后被误判空流——可见聚合必须计入。
func TestItemDoneAggregateMessageNotMisclassified(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"data: " + `{"type":"response.output_item.done","item":{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"the full answer arrives only here"}]}}`,
		"data: " + `{"type":"response.completed","response":{"id":"resp_x"}}`,
		"",
	}, "\n\n")
	state := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&state, []byte(body))
	if state.aggregateRunes == 0 {
		t.Fatal("item.done aggregate message text must count as visible output")
	}
	if state.emptyEvidence() {
		t.Fatal("aggregate-only stream must not classify as empty")
	}
}

// TestItemDoneFunctionCallCountsSemantic：工具调用 item 经 item.done 送达
// 时按语义输出处理（与 completed 帧聚合同语义）。
func TestItemDoneFunctionCallCountsSemantic(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"data: " + `{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":"{}"}}`,
		"",
	}, "\n\n")
	state := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&state, []byte(body))
	if !state.semanticOutput {
		t.Fatal("item.done function_call must count as semantic output")
	}
	_ = io.Discard
}
