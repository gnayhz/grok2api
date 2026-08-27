package rsc

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 空白首包不得定罪:上游可能先推一个空/空白 RESPONSE 再推 thinking,
// 此前空白首包立即按 denied 结束会误判健康账号。
func TestSSOProbeBlankAnswerThenThinkingIsClean(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelAssistantText, "  \n "),
		chunkEvent(channelReasoning, "thinking arrives after a blank preamble"),
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictClean {
		t.Fatalf("blank answer followed by thinking = %#v, want clean", result)
	}
}

// 全程只有空白答案、无任何 thinking:不可判定(error),绝不定罪。
func TestSSOProbeWhitespaceOnlyAnswerIsInconclusive(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		chunkEvent(channelAssistantText, " \n\t "),
		{"type": "response.done"},
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError {
		t.Fatalf("whitespace-only answer = %#v, want error", result)
	}
}

// turn 之后零 chunk、服务端直接关连接:传输面无信息,error。
func TestSSOProbeZeroChunksAfterTurnIsInconclusive(t *testing.T) {
	server := probeServer(t, &probeServerScript{})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError {
		t.Fatalf("zero chunks = %#v, want error", result)
	}
	if result.Error == "" || !strings.Contains(result.Error, "stream ended") {
		t.Fatalf("error = %q, want stream-ended detail", result.Error)
	}
}

// response.done 在任何可判定 chunk 之前到达:error,绝不当 risk。
func TestSSOProbeResponseDoneBeforeEvidenceIsInconclusive(t *testing.T) {
	server := probeServer(t, &probeServerScript{chunks: []map[string]any{
		{"type": "response.done"},
	}})
	defer server.Close()
	result := probeChecker(server).Check(context.Background(), "token-1")
	if result.Verdict != VerdictError {
		t.Fatalf("response.done before evidence = %#v, want error", result)
	}
}

// 读超时(首包静默超过 Timeout):error。超时/部分流是传输故障,按探针
// 纪律记 error 重试,绝不判 risk。
func TestSSOProbeReadDeadlineIsInconclusive(t *testing.T) {
	server := probeServer(t, &probeServerScript{delay: 2 * time.Second, chunks: []map[string]any{
		chunkEvent(channelNotetakerHeader, "too late"),
	}})
	defer server.Close()
	checker := NewSSOProbeChecker(500 * time.Millisecond)
	checker.baseURL = server.URL
	start := time.Now()
	result := checker.Check(context.Background(), "token-1")
	if result.Verdict != VerdictError {
		t.Fatalf("read deadline = %#v, want error", result)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("probe took %v, deadline should have fired near 500ms", elapsed)
	}
}
