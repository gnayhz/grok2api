package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestUsageClaimWithOutputTokensIsEmptyStream：A 审查指出的回归形态——
// terminal+usage 事件（completion_tokens>0）先到达、EOF 分离在后。零内容零
// 推理时 usage 声明不得把空流洗成“可扣留”，必须走空流路径。
func TestUsageClaimWithOutputTokensIsEmptyStream(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":9,"completion_tokens":57,"completion_tokens_details":{"reasoning_tokens":41}}}`,
		"data: [DONE]",
		"",
	}, "\n")
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolChat,
		QualityRetryRuntime{})
	if err == nil || verdict != QualityWait {
		t.Fatalf("usage-only stream with completion claim must be empty-stream, verdict=%s err=%v", verdict, err)
	}
	_ = replay.Close()
}
