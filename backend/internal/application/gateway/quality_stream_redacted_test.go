package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestStreamRedactedOnlyTakesEmptyStreamPath：流式 redacted-only 形态在
// peek 层与 body 层（round 46）同收口——空流路径（15m 空闲），不是
// missing-thinking 定罪。redacted 是密文不是证据，但也可能是健康账号的
// 隐私脱敏。round 32 的分类器断言是空流短路后的纵深防御。
func TestStreamRedactedOnlyTakesEmptyStreamPath(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"data: " + `{"type":"content_block_start","content_block":{"type":"redacted_thinking"}}`,
		"data: " + `{"type":"content_block_stop"}`,
		"data: " + `{"type":"message_stop"}`,
		"",
	}, "\n")
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolAnthropic, QualityRetryRuntime{})
	if !errors.Is(err, errQualityEmptyStream) || verdict != QualityWait {
		t.Fatalf("stream redacted-only verdict=%s err=%v, want wait + empty-stream (idle path, not conviction)", verdict, err)
	}
	if replay != nil {
		_ = replay.Close()
	}
}

// TestStreamRedactedOnlyStaysIdleWhenThinkingExpected：若误把
// redacted_thinking 标成 semanticOutput，思考期望内会走 emptyStreamVerdict
// 扣留（定罪）而不是空流空闲。隐私脱敏必须仍是 idle，不是 wash 也不是定罪。
func TestStreamRedactedOnlyStaysIdleWhenThinkingExpected(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"data: " + `{"type":"content_block_start","content_block":{"type":"redacted_thinking"}}`,
		"data: " + `{"type":"content_block_stop"}`,
		"data: " + `{"type":"message_stop"}`,
		"",
	}, "\n")
	cfg := QualityRetryRuntime{Enabled: true, ReasoningExpected: true}
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolAnthropic, cfg)
	if replay != nil {
		_ = replay.Close()
	}
	if !errors.Is(err, errQualityEmptyStream) || verdict != QualityWait {
		t.Fatalf("thinking-expected redacted-only verdict=%s err=%v, want wait + empty-stream (not semantic withhold)", verdict, err)
	}
	_, bodyVerdict, _, bodyErr := peekQualityBody(io.NopCloser(strings.NewReader(`{"content":[{"type":"redacted_thinking","data":"sig"}]}`)), cfg)
	if !errors.Is(bodyErr, errQualityEmptyStream) || bodyVerdict != QualityWait {
		t.Fatalf("thinking-expected redacted-only body verdict=%s err=%v, want wait + empty-stream", bodyVerdict, bodyErr)
	}
}
