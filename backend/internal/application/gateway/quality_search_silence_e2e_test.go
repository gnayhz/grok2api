package gateway

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestSearchSilentStreamDeliversThroughLoop：真实生产形态的端到端回归
// 部分上游变体在服务端搜索期间完全静默——连
// web_search_call item 头都不发，搜索结果只在 completed 帧出现。
// 带搜索工具的请求在短证据截止（80ms）下，300ms 搜索静默后正常
// 交付思考+正文；无搜索工具的同样静默仍被截止中止（对照组）。
func TestSearchSilentStreamDeliversThroughLoop(t *testing.T) {
	t.Parallel()
	silentSearchStream := func() io.ReadCloser {
		r, w := io.Pipe()
		go func() {
			defer w.Close()
			nl := "\n\n"
			_, _ = w.Write([]byte("data: " + `{"type":"response.created","response":{"id":"r_s"}}` + nl))
			time.Sleep(300 * time.Millisecond) // 搜索静默：截止的 3.7 倍，无任何 item 头
			_, _ = w.Write([]byte("data: " + `{"type":"response.reasoning_text.delta","delta":"plan after sources"}` + nl))
			_, _ = w.Write([]byte("data: " + `{"type":"response.output_text.delta","delta":"answer with sources"}` + nl))
			_, _ = w.Write([]byte("data: " + `{"type":"response.completed","response":{"id":"r_s","status":"completed","output":[{"type":"web_search_call","id":"ws_1"}]}}` + nl))
		}()
		return r
	}
	cfg := QualityRetryRuntime{Enabled: true, MaxAttempts: 1, EvidenceTimeout: 80 * time.Millisecond, CreatedTimeout: 500 * time.Millisecond}
	// 对照组：无搜索工具 + 静默 → 证据截止中止。
	_, plainVerdict, _, plainErr := peekQualityStream(context.Background(), silentSearchStream(), qualityProtocolResponses, cfg)
	if plainErr == nil || plainVerdict != QualityWait {
		t.Fatalf("plain silence must abort: verdict=%s err=%v", plainVerdict, plainErr)
	}
	// 主组：搜索请求的扫描器级预算豁免（service 层把 EvidenceTimeout 提到
	// qualitySearchSilenceBudget）。
	searchCfg := cfg
	searchCfg.EvidenceTimeout = qualitySearchSilenceBudget
	replay, verdict, _, err := peekQualityStream(context.Background(), silentSearchStream(), qualityProtocolResponses, searchCfg)
	if replay != nil {
		_ = replay.Close()
	}
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("search silence must deliver: verdict=%s err=%v", verdict, err)
	}
}
