package gateway

import (
	"context"
	"errors"
	"fmt"
	admpkg "github.com/chenyme/grok2api/backend/internal/application/admission"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestClassifyQualityHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sig  QualityStreamSignals
		want QualityVerdict
	}{
		{name: "thinking delivers", sig: QualityStreamSignals{HasThinking: true, VisibleTokens: 10}, want: QualityDeliver},
		// ReasoningTokens without HasThinking is a usage claim, not stream
		// evidence: degraded streams report large counts and must withhold.
		{name: "usage reasoning claim still withholds", sig: QualityStreamSignals{ReasoningTokens: 40, VisibleTokens: 80, Terminal: true}, want: QualityWithhold},
		{name: "visible 32 no think withhold", sig: QualityStreamSignals{VisibleTokens: 32, Terminal: true}, want: QualityWithhold},
		{name: "output 40 no think withhold", sig: QualityStreamSignals{OutputTokens: 40, Terminal: true}, want: QualityWithhold},
		{name: "short no think withholds (reasoning models always think)", sig: QualityStreamSignals{VisibleTokens: 10, Terminal: true}, want: QualityWithhold},
		{name: "empty terminal withholds defensively (empty-stream path owns it)", sig: QualityStreamSignals{Terminal: true}, want: QualityWithhold},
		{name: "midstream enough content withhold", sig: QualityStreamSignals{VisibleTokens: 64}, want: QualityWithhold},
		{name: "any visible output without thinking withholds (body outrun)", sig: QualityStreamSignals{VisibleTokens: 8}, want: QualityWithhold},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyQualityHoldShadowed(test.sig); got != test.want {
				t.Fatalf("classifyQualityHoldShadowed() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestDecideQualityRetry(t *testing.T) {
	t.Parallel()
	if got := admpkg.DecideRetry(QualityDeliver, 0, 2); got != QualityActionDeliver {
		t.Fatalf("deliver verdict: %s", got)
	}
	if got := admpkg.DecideRetry(QualityWithhold, 0, 2); got != QualityActionRetry {
		t.Fatalf("first withhold: %s", got)
	}
	// G12:耗尽策略只剩 fail_closed——扣留预算耗尽即 Reject,绝无 DeliverLast。
	if got := admpkg.DecideRetry(QualityWithhold, 1, 2); got != QualityActionReject {
		t.Fatalf("last withhold must reject (fail-closed only): %s", got)
	}
	if got := admpkg.DecideRetry(QualityWithhold, 0, 1); got != QualityActionReject {
		t.Fatalf("max 1 must reject: %s", got)
	}
	if got := admpkg.DecideRetry(QualityWithhold, 5, 0); got != QualityActionReject {
		t.Fatalf("zero-value config must fail closed: %s", got)
	}
}

func TestDecideQualityRetryLastWithholdIsMaxAttemptsMinusOne(t *testing.T) {
	t.Parallel()
	for _, maxAttempts := range []int{1, 2, 3, 6} {
		last := maxAttempts - 1
		if got := admpkg.DecideRetry(QualityWithhold, last, maxAttempts); got != QualityActionReject {
			t.Fatalf("last withhold must reject max=%d index=%d got %s", maxAttempts, last, got)
		}
		if last > 0 {
			if got := admpkg.DecideRetry(QualityWithhold, last-1, maxAttempts); got != QualityActionRetry {
				t.Fatalf("pre-last should retry max=%d index=%d got %s", maxAttempts, last-1, got)
			}
		}
	}
}

func TestCommitQualityHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		verdict        QualityVerdict
		qualityAttempt int
		maxAttempts    int
		hasNext        bool
		wantAction     QualityRetryAction
		wantAudit      bool
		wantKeep       bool
	}{
		{
			name:    "first withhold + hasNext → Retry+Audit",
			verdict: QualityWithhold, qualityAttempt: 0, maxAttempts: 2, hasNext: true,
			wantAction: QualityActionRetry, wantAudit: true, wantKeep: false,
		},
		{
			name:    "last withhold → Reject+Audit (fail-closed only, G12)",
			verdict: QualityWithhold, qualityAttempt: 1, maxAttempts: 2, hasNext: true,
			wantAction: QualityActionReject, wantAudit: true, wantKeep: false,
		},
		{
			name:    "routing exhausted even at qualityAttempt=0 → Reject, never Retry",
			verdict: QualityWithhold, qualityAttempt: 0, maxAttempts: 2, hasNext: false,
			wantAction: QualityActionReject, wantAudit: true, wantKeep: false,
		},
		{
			name:    "thinking delivers keep body",
			verdict: QualityDeliver, qualityAttempt: 0, maxAttempts: 2, hasNext: true,
			wantAction: QualityActionDeliver, wantAudit: false, wantKeep: true,
		},
		{
			name:    "switch 5 times: attempt 4 of 6 still retries",
			verdict: QualityWithhold, qualityAttempt: 4, maxAttempts: 6, hasNext: true,
			wantAction: QualityActionRetry, wantAudit: true, wantKeep: false,
		},
		{
			name:    "switch 5 times: attempt 5 of 6 rejects no body",
			verdict: QualityWithhold, qualityAttempt: 5, maxAttempts: 6, hasNext: true,
			wantAction: QualityActionReject, wantAudit: true, wantKeep: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := commitQualityHold(test.verdict, test.qualityAttempt, test.maxAttempts, test.hasNext)
			if got.Action != test.wantAction || got.Audit != test.wantAudit || got.KeepBody != test.wantKeep {
				t.Fatalf("commitQualityHold() = %+v, want action=%s audit=%t keep=%t", got, test.wantAction, test.wantAudit, test.wantKeep)
			}
			if got.Action == QualityActionRetry && !test.hasNext {
				t.Fatal("routing exhausted must not Retry")
			}
		})
	}
}

func TestBoundQualityRetryWhenRoutingExhausted(t *testing.T) {
	t.Parallel()
	if got := admpkg.BoundRetry(QualityActionRetry, true); got != QualityActionRetry {
		t.Fatalf("has next: %s", got)
	}
	if got := admpkg.BoundRetry(QualityActionRetry, false); got != QualityActionReject {
		t.Fatalf("no next must reject (fail-closed only): %s", got)
	}
	if got := admpkg.BoundRetry(QualityActionDeliver, false); got != QualityActionDeliver {
		t.Fatalf("non-retry passthrough: %s", got)
	}
}

func TestObserveQualityChunkThinkingChat(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolChat}
	observeQualityChunk(&state, []byte(sse(
		`data: {"choices":[{"delta":{"thinking_content":"plan the game"}}]}`,
		`data: {"choices":[{"delta":{"content":"here is a game"}}]}`,
		`data: {"usage":{"completion_tokens":80,"completion_tokens_details":{"reasoning_tokens":40}}}`,
		"data: [DONE]",
	)))
	sig := state.signals()
	if !sig.HasThinking || !sig.Terminal || sig.ReasoningTokens != 40 {
		t.Fatalf("thinking fixture signals = %#v", sig)
	}
	if classifyQualityHoldShadowed(sig) != QualityDeliver {
		t.Fatalf("thinking fixture withheld")
	}
}

// TestObserveOutputWithoutThinkingWithholdsAnyLength：正文抢跑规则与长度
// 无关——无思考的正文不论 200 rune 还是 2 rune 都扣留。两条用例源自
// minOutputTokens 阈值时代（enough/short 各测一侧），阈值删除后合并为
// 同一条长度无关性锁定；大尺寸侧同时锁定扫描器的可见输出记账。
func TestObserveOutputWithoutThinkingWithholdsAnyLength(t *testing.T) {
	t.Parallel()
	large := qualityScanState{protocol: qualityProtocolChat}
	content := strings.Repeat("word ", 40) // 200 runes → 50 tokens
	observeQualityChunk(&large, []byte(sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"usage":{"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)))
	sig := large.signals()
	if sig.HasThinking || !sig.Terminal || sig.VisibleTokens < 32 {
		t.Fatalf("no-think fixture signals = %#v", sig)
	}
	if classifyQualityHoldShadowed(sig) != QualityWithhold {
		t.Fatalf("no-think output must withhold, got %s (%#v)", classifyQualityHoldShadowed(sig), sig)
	}
	// Even a 1-token answer normally carries reasoning: no thinking at
	// terminal is degraded regardless of length and must withhold.
	short := qualityScanState{protocol: qualityProtocolChat}
	observeQualityChunk(&short, []byte(sse(
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		"data: [DONE]",
	)))
	if classifyQualityHoldShadowed(short.signals()) != QualityWithhold {
		t.Fatalf("short no-think must withhold, got %s", classifyQualityHoldShadowed(short.signals()))
	}
}

func TestObserveQualityChunkResponsesReasoningItem(t *testing.T) {
	t.Parallel()
	// A reasoning item header without any reasoning text delta is the
	// responses-protocol B-form: withhold despite the usage claim.
	state := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&state, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	if classifyQualityHoldShadowed(state.signals()) != QualityWithhold {
		t.Fatalf("responses reasoning item header alone must withhold: %#v", state.signals())
	}
}

func TestObserveQualityChunkResponsesReasoningDeltaDelivers(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&state, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.reasoning_text.delta","delta":"consider the options"}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	if classifyQualityHoldShadowed(state.signals()) != QualityDeliver {
		t.Fatalf("responses reasoning text delta should deliver: %#v", state.signals())
	}
}

func TestPeekQualityStreamThinkingDeliversRemainder(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader(sse(
		`data: {"choices":[{"delta":{"thinking_content":"think"}}]}`,
		`data: {"choices":[{"delta":{"content":"answer after think"}}]}`,
		"data: [DONE]",
	)))
	replay, verdict, _, err := peekQualityStream(context.Background(), body, qualityProtocolChat, QualityRetryRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("verdict=%s", verdict)
	}
	got, _ := io.ReadAll(replay)
	if !strings.Contains(string(got), "answer after think") || !strings.Contains(string(got), "thinking_content") {
		t.Fatalf("replay lost frames: %s", got)
	}
}

// TestPeekWithholdsOutputWithoutThinkingAnyLength：正文抢跑扣留与长度无关
// （阈值时代的 enough/short 两条合并）；大尺寸侧同时锁定 usage 记账。
func TestPeekWithholdsOutputWithoutThinkingAnyLength(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcd", 40) // 160 runes → 40 tokens
	body := io.NopCloser(strings.NewReader(sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)))
	replay, verdict, usage, err := peekQualityStream(context.Background(), body, qualityProtocolChat, QualityRetryRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("verdict=%s usage=%#v", verdict, usage)
	}
	if usage.Reported {
		t.Fatalf("the later usage frame must not delay interception: %#v", usage)
	}
	// 2-rune 答案同样必须扣留：无思考的正文不论长短都是降智形态。
	short := io.NopCloser(strings.NewReader(sse(
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		"data: [DONE]",
	)))
	sReplay, sVerdict, _, sErr := peekQualityStream(context.Background(), short, qualityProtocolChat, QualityRetryRuntime{})
	if sErr != nil {
		t.Fatal(sErr)
	}
	defer sReplay.Close()
	if sVerdict != QualityWithhold {
		t.Fatalf("short no-think verdict=%s, want withhold", sVerdict)
	}
}

func TestPeekThenDecideQualityRetryBounded(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcd", 40)
	fixture := sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)
	cfg := QualityRetryRuntime{MaxAttempts: 2, OnExhausted: qualityRetryFailClosed}

	replay, verdict, usage, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(fixture)), qualityProtocolChat, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("first peek verdict=%s usage=%#v", verdict, usage)
	}
	if got := admpkg.DecideRetry(verdict, 0, cfg.MaxAttempts); got != QualityActionRetry {
		t.Fatalf("first withhold action=%s", got)
	}

	replay2, verdict2, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(fixture)), qualityProtocolChat, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replay2.Close()
	if verdict2 != QualityWithhold {
		t.Fatalf("second peek verdict=%s", verdict2)
	}
	// G12:耗尽即 Reject——无下一跳时同样 Reject,绝无 DeliverLast 兜底。
	action2 := admpkg.DecideRetry(verdict2, 1, cfg.MaxAttempts)
	action2 = admpkg.BoundRetry(action2, false)
	if action2 != QualityActionReject {
		t.Fatalf("second withhold exhausted action=%s", action2)
	}
	got, _ := io.ReadAll(replay2)
	if !strings.Contains(string(got), content) {
		t.Fatalf("replay body must still be readable for audit, got %q", got)
	}
}

func TestPeekWithholdClosesUpstreamImmediately(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	first := sse(`data: {"choices":[{"delta":{"content":"hi"}}]}`)
	second := sse(`data: {"choices":[{"delta":{"content":" after timeout"}}]}`, "data: [DONE]")
	writeErr := make(chan error, 1)
	continueWrite := make(chan struct{})
	go func() {
		if _, err := io.WriteString(writer, first); err != nil {
			writeErr <- err
			return
		}
		select {
		case <-continueWrite:
		case <-time.After(500 * time.Millisecond):
		}
		if _, err := io.WriteString(writer, second); err != nil {
			writeErr <- err
			return
		}
		writeErr <- writer.Close()
	}()

	started := time.Now()
	replay, verdict, _, err := peekQualityStream(context.Background(), reader, qualityProtocolChat, QualityRetryRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	close(continueWrite)
	// 正文抢跑规则（蓝图规则 3）：无思考的首个正文增量即刻扣留，无需等
	// 30ms hold 窗口走完——零延迟状态机的核心承诺。
	// 界限 500ms：正文抢跑必须即时扣留（不等任何截止），同时对 -race
	// 高负载下的调度噪声免疫（旧 50ms 硬界限偶发翻牌）。
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("peek returned after %s, want immediate withhold on first content delta", elapsed)
	}
	if verdict != QualityWithhold {
		t.Fatalf("short timed-out no-think response verdict = %s, want withhold", verdict)
	}
	body, err := io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("withhold must abort the producer before returning: %v", err)
	}
	if got := string(body); !strings.Contains(got, "hi") || strings.Contains(got, "after timeout") {
		t.Fatalf("withheld replay should contain only the observed prefix: %q", got)
	}
}

func TestPeekEmptyStreamWaitsForDeadlineInsteadOfFailingOpen(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan struct{})
	var verdict QualityVerdict
	var peekErr error
	go func() {
		defer close(done)
		_, verdict, _, peekErr = peekQualityStream(ctx, reader, qualityProtocolChat, QualityRetryRuntime{})
	}()
	select {
	case <-done:
		t.Fatal("empty hold timeout must keep reading, not fail-open")
	case <-time.After(50 * time.Millisecond):
	}
	cancel(neterrorpkg.ErrUpstreamStreamIdleTimeout)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peekQualityStream did not return after idle cancel")
	}
	if !neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) {
		t.Fatalf("peekErr = %v, want idle timeout", peekErr)
	}
	if verdict != QualityWait {
		t.Fatalf("verdict=%s, want wait so the loop does not fail-open", verdict)
	}
}

func TestPeekQualityStreamEmptyEOFRequestsAnotherAccount(t *testing.T) {
	t.Parallel()
	replay, verdict, _, err := peekQualityStream(
		context.Background(),
		io.NopCloser(strings.NewReader("")),
		qualityProtocolResponses,
		QualityRetryRuntime{},
	)
	if replay != nil {
		defer replay.Close()
	}
	if !errors.Is(err, errQualityEmptyStream) {
		t.Fatalf("peek error = %v, want empty stream", err)
	}
	if verdict != QualityWait {
		t.Fatalf("verdict = %s, want wait", verdict)
	}
}

func TestPeekQualityStreamProcessesUnterminatedFinalEvent(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader(`data: {"type":"response.output_text.delta","delta":"ok"}`))
	replay, verdict, _, err := peekQualityStream(
		context.Background(), body, qualityProtocolResponses,
		QualityRetryRuntime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	// Unterminated or not, a finished stream without thinking is degraded:
	// reasoning models think even on one-token answers.
	if verdict != QualityWithhold {
		t.Fatalf("verdict = %s, want withhold for a real short no-think response", verdict)
	}
}

func TestQualityPeekAbortErrorPrefersIdleCause(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(neterrorpkg.ErrUpstreamStreamIdleTimeout)
	got := qualityPeekAbortError(ctx, context.Canceled)
	if !neterrorpkg.IsUpstreamStreamIdleTimeout(got) {
		t.Fatalf("abort error = %v, want idle timeout", got)
	}
	if isClientRequestCancel(ctx, got) {
		t.Fatal("idle timeout must not look like a client cancel")
	}
	plain, plainCancel := context.WithCancel(context.Background())
	plainCancel()
	if !isClientRequestCancel(plain, context.Canceled) {
		t.Fatal("plain cancel must still be a client cancel")
	}
}

func TestShouldHoldQualityStreamGates(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, MaxAttempts: 2}
	route := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.6", PublicID: "grok-4.6"}
	input := Input{Streaming: true, PublicModel: "grok-4.6"}
	if !shouldHoldQualityStream(input, nil, route, audit.OperationChat, cfg, nil) {
		t.Fatal("expected hold on thinking build chat")
	}
	nonStreamChat := input
	nonStreamChat.Streaming = false
	if !shouldHoldQualityStream(nonStreamChat, nil, route, audit.OperationChat, cfg, nil) {
		t.Fatal("non-stream chat must hold")
	}
	off := cfg
	off.Enabled = false
	if shouldHoldQualityStream(input, nil, route, audit.OperationChat, off, nil) {
		t.Fatal("disabled must not hold")
	}
	owned := inferencedomain.ResponseOwnership{ResponseID: "r1", AccountID: 1}
	if !shouldHoldQualityStream(input, &owned, route, audit.OperationChat, cfg, nil) {
		t.Fatal("pinned previous_response_id must still hold")
	}
	// 非推理操作族全部走同一豁免分支——逐一钉住，防止未来有人在门里
	// 加操作白名单时漏掉某个媒体操作（video/media/tts/embedding）。
	for _, operation := range []audit.Operation{audit.OperationImage, audit.OperationImageEdit, audit.OperationVideo, audit.OperationTTS, audit.OperationSTT, audit.OperationRealtime, audit.OperationVoice} {
		if shouldHoldQualityStream(input, nil, route, operation, cfg, nil) {
			t.Fatalf("%s must not hold", operation)
		}
	}
	if shouldHoldQualityStream(input, nil, route, audit.OperationCompaction, cfg, nil) {
		t.Fatal("codex compaction operation must not hold")
	}
	classified := input
	classified.skipQualityHold = true
	if shouldHoldQualityStream(classified, nil, route, audit.OperationResponses, cfg, nil) {
		t.Fatal("gateway-classified compaction must not hold")
	}
	tui := input
	tui.Body = []byte(`{"input":[{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`)
	if shouldHoldQualityStream(tui, nil, route, audit.OperationResponses, cfg, nil) {
		t.Fatal("tui compaction prompt must not hold even when tagged responses")
	}
	// reasoning_disabled 豁免已删除：守卫白名单内的模型
	//（grok-4.5/4.6）均不支持 none——显式关闭是非法组合，照常进守卫
	//（上游将以 400 拒绝，判决无从发生也不误罚账号）；白名单外的模型由
	// model_out_of_scope 整体豁免。锁定：显式关闭不再构成豁免。
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "chat reasoning none", body: `{"reasoning_effort":"none"}`},
		{name: "responses reasoning none", body: `{"reasoning":{"effort":"none"}}`},
		{name: "messages thinking disabled", body: `{"thinking":{"type":"disabled"}}`},
		{name: "messages zero thinking budget", body: `{"thinking":{"type":"enabled","budget_tokens":0}}`},
		{name: "output_config effort none", body: `{"output_config":{"effort":"none"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if !shouldHoldQualityStream(request, nil, route, audit.OperationChat, cfg, nil) {
				t.Fatal("explicit disable on none-incapable model must stay gated (exemption removed)")
			}
		})
	}
	// Grok TUI attaches a tools schema to every agent turn, including the
	// first thinking-only one: a schema declaration alone must NOT exempt the
	// request, otherwise all TUI traffic escapes the guard.
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "client tools schema", body: `{"tools":[{"type":"function","function":{"name":"charge"}}]}`},
		{name: "legacy functions schema", body: `{"functions":[{"name":"charge"}]}`},
		{name: "tui tools schema plus user input", body: `{"tools":[{"type":"function","name":"read_file"}],"input":[{"role":"user","content":"hello"}]}`},
		// Build cache-route injection on a tool-free chat: web_search+x_search
		// with tool_choice none. Upstream tees show those tools; hold must
		// still gate on stream features, not treat the injection as a skip.
		{name: "injected web_search x_search choice none", body: `{"tools":[{"type":"web_search"},{"type":"x_search"}],"tool_choice":"none"}`},
		// 用户消息文本里引用工具标记字样：JSON 序列化转义后不含干净的字面量
		// （\"...\" 不产生完整匹配），预检短路为"无工具结果"——必须仍然 hold。
		{name: "escaped marker in message text", body: `{"input":[{"role":"user","content":"pass \"function_call_output\" and \"tool_result\" in a string"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if !shouldHoldQualityStream(request, nil, route, audit.OperationChat, cfg, nil) {
				t.Fatal("tools schema declaration alone must still hold so TUI thinking turns are classified")
			}
		})
	}
	// 带工具结果的轮次照常 hold(回归锁定):扣留的响应从不发给
	// 客户端,不存在客户端重放;判决只看这条响应自身的流特征。
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "function_call_output", body: `{"input":[{"role":"user","content":"hi"},{"type":"function_call_output","call_id":"c1","output":"done"}]}`},
		{name: "tool_result", body: `{"input":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}`},
		{name: "tool_use_output", body: `{"input":[{"type":"tool_use_output","output":"done"}]}`},
		{name: "role tool message", body: `{"messages":[{"role":"user","content":"hi"},{"role":"tool","content":"done"}]}`},
		{name: "nested tool output", body: `{"input":[{"role":"user","content":[{"type":"tool_result","content":"done"}]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if !shouldHoldQualityStream(request, nil, route, audit.OperationChat, cfg, nil) {
				t.Fatal("tool results in context must not exempt the hold (stream-characteristic verdict)")
			}
		})
	}
	toolCache := input
	toolCache.Body = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	toolCache.PromptCacheKey = "client-cache-identity"
	if !shouldHoldQualityStream(toolCache, nil, route, audit.OperationChat, cfg, nil) {
		t.Fatal("client identity/cache compatibility alone must not disable the hold")
	}
}

func TestAttemptLoopQualityHold(t *testing.T) {
	ctx := context.Background()
	database, credentials := newGuardLoopDatabase(t, "grok-4.6", "quality-empty", "quality-no-think", "quality-thinking")
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{ModelScope: clientkey.ModelScopeAll,
		Name: "quality-loop-key", Prefix: "qhold", SecretHash: strings.Repeat("f", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	noThink := sse(
		`data: {"choices":[{"delta":{"content":"`+strings.Repeat("abcd", 40)+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)
	thinking := sse(
		`data: {"choices":[{"delta":{"thinking_content":"plan the game"}}]}`,
		`data: {"choices":[{"delta":{"content":"good game after retry"}}]}`,
		`data: {"usage":{"completion_tokens":80,"completion_tokens_details":{"reasoning_tokens":40}}}`,
		"data: [DONE]",
	)
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusOK, body: ""}},
		credentials[1].ID: {{status: http.StatusOK, body: noThink}},
		credentials[2].ID: {{status: http.StatusOK, body: thinking}},
	}}
	registry := providerimpl.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), security.RandomTokenSource{}, nil, nil, nil)
	sel := selector.NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService("test-owner", nil, nil, nil, 60, 4, nil, security.RandomTokenSource{}), registry, sel, historyapp.NewResponseResources(responseRepo), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 3)
	service.SetGuardSnapshotSource(StaticGuardSnapshotSource(QualityRetryRuntime{Enabled: true, MaxAttempts: 3, OnExhausted: qualityRetryFailClosed, GuardedModels: []string{"grok-4.6"}}))

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-quality-hold", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"write a game"}],"stream":true}`),
	})
	if err != nil {
		t.Fatalf("attempt loop should deliver after withhold retry, err=%v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	body, _ := io.ReadAll(result.Body)
	finishTestResult(t, result, Usage{Reported: true, OutputTokens: 80, ReasoningTokens: 40}, "chat-ok", "")
	_ = result.Body.Close()
	if !strings.Contains(string(body), "good game after retry") || !strings.Contains(string(body), "thinking_content") {
		t.Fatalf("client must receive the second attempt body, got %s", body)
	}
	if strings.Contains(string(body), strings.Repeat("abcd", 40)) {
		t.Fatal("first no-think body must not be delivered")
	}
	attempts := adapter.Attempts()
	if len(attempts) != 3 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID || attempts[2] != credentials[2].ID {
		t.Fatalf("expected empty+no-think account exclusion and retry, attempts=%#v", attempts)
	}
	emptyAccount, err := accountRepo.Get(ctx, credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if emptyAccount.FailureCount != 0 || emptyAccount.CooldownUntil == nil {
		t.Fatalf("empty stream account was not cooled: %#v", emptyAccount)
	}
	if emptyAccount.LastError != accountdomain.LastErrorQualityIdle {
		t.Fatalf("empty stream cooldown marker = %q, want quality_idle_timeout", emptyAccount.LastError)
	}
	if remaining := time.Until(*emptyAccount.CooldownUntil); remaining < 14*time.Minute || remaining > 15*time.Minute+time.Second {
		t.Fatalf("empty stream cooldown = %s, want about 15m", remaining)
	}
	noThinkingAccount, err := accountRepo.Get(ctx, credentials[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !noThinkingAccount.Enabled || noThinkingAccount.LastError != "" || noThinkingAccount.CooldownUntil != nil {
		t.Fatalf("quality hold changed manual or health state: %#v", noThinkingAccount)
	}
	if sel.LocalQualityAllowed(noThinkingAccount.ID, time.Now()) {
		t.Fatal("rejected account missing temporary hold")
	}

	logs, total, err := auditRepo.List(ctx, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var deliveredID uint64
	parents := 0
	for _, rec := range logs {
		if rec.RequestID != "req-quality-hold" {
			continue
		}
		parents++
		if rec.ErrorCode == ErrorQualityDegraded {
			t.Fatalf("withhold must not create a second parent row, got status=%d error=%s", rec.StatusCode, rec.ErrorCode)
		}
		if rec.ErrorCode == "" && rec.StatusCode == http.StatusOK {
			deliveredID = rec.ID
		}
	}
	if parents != 1 {
		t.Fatalf("one request must write one audit parent, got %d (total rows %d)", parents, total)
	}
	if deliveredID == 0 {
		t.Fatal("final delivered attempt missing from audits")
	}
	detail, err := auditRepo.Get(ctx, deliveredID)
	if err != nil {
		t.Fatal(err)
	}
	foundHold := false
	for _, attempt := range detail.Attempts {
		if attempt.TransportError == ErrorQualityDegraded {
			foundHold = true
			break
		}
	}
	if !foundHold {
		t.Fatalf("withhold must appear as an attempt on the delivered request, attempts=%d", len(detail.Attempts))
	}
}

func TestAttemptLoopQualityHoldSingleAccountRejectsWhenExhausted(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-hold-single.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)

	credential, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "quality-only", SourceKey: "quality-only",
		EncryptedAccessToken: "quality-only", EncryptedRefreshToken: "refresh-quality-only",
		ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 200, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Discover(ctx, modelRepo, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Capabilities(ctx, modelRepo, accountRepo, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{ModelScope: clientkey.ModelScopeAll,
		Name: "quality-single-key", Prefix: "qsingle", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	content := strings.Repeat("abcd", 40)
	noThink := sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credential.ID: {{status: http.StatusOK, body: noThink}},
	}}
	registry := providerimpl.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), security.RandomTokenSource{}, nil, nil, nil)
	sel := selector.NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService("test-owner", nil, nil, nil, 60, 4, nil, security.RandomTokenSource{}), registry, sel, historyapp.NewResponseResources(responseRepo), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 999)
	service.SetGuardSnapshotSource(StaticGuardSnapshotSource(QualityRetryRuntime{
		Enabled: true, MaxAttempts: 6, OnExhausted: qualityRetryFailClosed, GuardedModels: []string{"grok-4.6"},
	}))

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-quality-single", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"write a game"}],"stream":true}`),
	})
	// G12:唯一账号扣留且无下一跳 ⇒ Reject(503),绝无 fail-open 交付。
	if err == nil {
		_ = result.Body.Close()
		t.Fatal("exhausted withhold must reject, got a delivered response")
	}
	if !errors.Is(err, errQualityDegraded) {
		t.Fatalf("reject cause must be quality degraded: %v", err)
	}
	if attempts := adapter.Attempts(); len(attempts) != 1 || attempts[0] != credential.ID {
		t.Fatalf("single-account pool must not enter a fake retry, attempts=%#v", attempts)
	}
	cooled, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cooled.Enabled || cooled.LastError != "" || cooled.CooldownUntil != nil {
		t.Fatalf("quality hold changed manual or health state: %#v", cooled)
	}
	if sel.LocalQualityAllowed(cooled.ID, time.Now()) {
		t.Fatal("rejected account missing temporary hold")
	}
}

func TestAttemptLoopQualityRejectAndTotalAttemptCap(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-hold-fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)

	credentials := make([]accountdomain.Credential, 0, 7)
	for index := 0; index < 7; index++ {
		name := fmt.Sprintf("quality-fallback-%d", index)
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name,
			EncryptedAccessToken: name, EncryptedRefreshToken: "refresh-" + name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
			Priority: 300 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := testsupport.Discover(ctx, modelRepo, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := testsupport.Capabilities(ctx, modelRepo, accountRepo, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{ModelScope: clientkey.ModelScopeAll,
		Name: "quality-fallback-key", Prefix: "qfallback", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	content := strings.Repeat("fallback", 24)
	responses := make(map[uint64][]scriptedBuildResponse, len(credentials))
	responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":48,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)}}
	for _, credential := range credentials[1:] {
		responses[credential.ID] = []scriptedBuildResponse{{status: http.StatusInternalServerError, body: `{"error":"temporary"}`}}
	}
	adapter := &scriptedBuildAdapter{responses: responses}
	registry := providerimpl.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), security.RandomTokenSource{}, nil, nil, nil)
	sel := selector.NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService("test-owner", nil, nil, nil, 60, 4, nil, security.RandomTokenSource{}), registry, sel, historyapp.NewResponseResources(responseRepo), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 999)
	service.SetGuardSnapshotSource(StaticGuardSnapshotSource(QualityRetryRuntime{
		Enabled: true, MaxAttempts: 6, OnExhausted: qualityRetryFailClosed, GuardedModels: []string{"grok-4.6"},
	}))

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-quality-fallback", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"write a game"}],"stream":true}`),
	})
	// G12:首个扣留即 Reject(无 DeliverLast 兜底);后续账号的传输失败照常
	// 占满 attempt 预算,请求以最终失败收场,不再保留任何降智响应。
	if err == nil {
		_ = result.Body.Close()
		t.Fatal("exhausted withhold must reject, got a delivered response")
	}
	if attempts := adapter.Attempts(); len(attempts) != 6 || attempts[0] != credentials[0].ID {
		t.Fatalf("requestRetry must cap real account attempts at 6, attempts=%#v", attempts)
	}
	// I5:同一脏路径重试恢复率≈0——降智触发的重试必须换号,
	// 6 次尝试全部落在不同账号上(同号重试已废除 G14)。
	seen := map[uint64]bool{}
	for _, id := range adapter.Attempts() {
		if seen[id] {
			t.Fatalf("withhold retry must rotate accounts, revisited %d in %#v", id, adapter.Attempts())
		}
		seen[id] = true
	}
	logs, _, err := auditRepo.List(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range logs {
		if record.StatusCode == http.StatusOK && record.AccountID != nil && *record.AccountID == credentials[0].ID {
			t.Fatal("the withheld degraded attempt must not be delivered or audited as success")
		}
	}
}

func TestNormalizeQualityRetryDefaults(t *testing.T) {
	t.Parallel()
	got := normalizeQualityRetry(QualityRetryRuntime{Enabled: true})
	if !got.Enabled || got.MaxAttempts != 2 || got.OnExhausted != qualityRetryFailClosed || got.AccountCooldown != 2*time.Minute || got.IdleAccountCooldown != 15*time.Minute {
		t.Fatalf("defaults = %#v", got)
	}
	if got.EvidenceTimeout != 3500*time.Millisecond {
		t.Fatalf("EvidenceTimeout default = %s, want 3.5s", got.EvidenceTimeout)
	}
	if got.CreatedTimeout != 5*time.Second {
		t.Fatalf("CreatedTimeout default = %s, want 5s", got.CreatedTimeout)
	}
	if len(got.GuardedModels) != 0 {
		t.Fatalf("empty GuardedModels must stay empty, got %#v", got.GuardedModels)
	}
	want := []string{"grok-4.5", "grok-4.6"}
	passthrough := normalizeQualityRetry(QualityRetryRuntime{Enabled: true, GuardedModels: want})
	if len(passthrough.GuardedModels) != 2 || passthrough.GuardedModels[0] != want[0] || passthrough.GuardedModels[1] != want[1] {
		t.Fatalf("GuardedModels passthrough = %#v, want %#v", passthrough.GuardedModels, want)
	}
}
