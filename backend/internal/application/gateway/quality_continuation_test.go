package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestContinuationStreamsFollowStandardRules：续写对话（历史含工具调用/
// 思考轮次）的流与其他请求共用同一套证据规则——语料复核推翻
// 了规模轮 16 的「续写轮可合法零思考」假设（未指定强度 136/153、显式低
// 强度 12/18 均产生思考，零思考仅出现在降智时刻），续写豁免删除
// （曾放行 17 条零思考交付，REASONING0_LEDGER §C2/C3）。行为锁：
// 无思考正文一律扣留；可见思考一律放行——与是否续写无关。
// （历史注记：续写检测管道连同豁免一并移除，schedule 不再解析请求历史。）
func TestContinuationStreamsFollowStandardRules(t *testing.T) {
	t.Parallel()
	continuationBody := `{"messages":[{"role":"user","content":"查天气"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"get_weather","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"22C"}]}]}`
	cfg := qualityLivenessSchedule([]byte(continuationBody), "messages", QualityRetryRuntime{Enabled: true, CreatedTimeout: 5 * time.Second, EvidenceTimeout: 3500 * time.Millisecond})
	// 续写流：无思考直接正文 → 规则 3 扣留。
	stream := strings.Join([]string{
		"data: " + `{"type":"response.created","response":{"id":"r_c"}}`,
		"data: " + `{"type":"response.output_item.added","item":{"id":"m1","type":"message"}}`,
		"data: " + `{"type":"response.output_text.delta","delta":"建议穿轻便外套"}`,
		"data: " + `{"type":"response.completed","response":{"id":"r_c","status":"completed"}}`,
		"",
	}, "\n\n")
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(stream)), qualityProtocolResponses, cfg)
	if replay != nil {
		_ = replay.Close()
	}
	if err != nil || verdict != QualityWithhold {
		t.Fatalf("continuation text-without-thinking must withhold: verdict=%s err=%v", verdict, err)
	}
	// 续写 + 有思考 → 规则 1 照常放行（健康续写流的真实形态）。
	thinkingStream := strings.Join([]string{
		"data: " + `{"type":"response.created","response":{"id":"r_t"}}`,
		"data: " + `{"type":"response.reasoning_summary_text.delta","delta":"对比体感温度"}`,
		"data: " + `{"type":"response.output_text.delta","delta":"建议穿轻便外套"}`,
		"data: " + `{"type":"response.completed","response":{"id":"r_t","status":"completed"}}`,
		"",
	}, "\n\n")
	replayT, verdictT, _, errT := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(thinkingStream)), qualityProtocolResponses, cfg)
	if replayT != nil {
		_ = replayT.Close()
	}
	if errT != nil || verdictT != QualityDeliver {
		t.Fatalf("continuation with visible thinking must deliver: verdict=%s err=%v", verdictT, errT)
	}
}
