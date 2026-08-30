package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestZeroDelayWithholdOnCiphertextItemDone 端到端计时证据（蓝图 §六 KPI
// 「降智拦截判定耗时 < 0.01s」）：降智流形态——response.created 后直接
// output_item.done 携带数 KiB encrypted_content 密文、零思考增量——必须在
// 远小于任何超时预算的时间内被判定 QualityWithhold，而不是死等
// EvidenceTimeout。
func TestZeroDelayWithholdOnCiphertextItemDone(t *testing.T) {
	t.Parallel()
	ciphertext := `"encrypted_content":"` + strings.Repeat("A", 64*1024) + `"`
	degraded := "data: " + `{"type":"response.created","response":{"id":"resp_deg"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","` + ciphertext[1:] + `}}` + "\n\n"
	started := time.Now()
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(degraded)), qualityProtocolResponses, QualityRetryRuntime{
		EvidenceTimeout: 3500 * time.Millisecond, CreatedTimeout: 5 * time.Second,
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("ciphertext item.done without thinking verdict = %s, want withhold", verdict)
	}
	// KPI：零延迟判定不得等待任何截止（旧路径死等 15s EvidenceTimeout）。
	// 界限取 1s——正常负载下实测微秒级（基准 72-460µs），而任何"等截止"
	// 的回归都会撞上 3.5s 证据预算；1s 界限对调度噪声免疫（
	// verify-full 曾在 -race 高负载下因 10ms 硬界限偶发翻牌）。
	if elapsed >= time.Second {
		t.Fatalf("zero-delay withhold took %s, want far below the 3.5s evidence deadline", elapsed)
	}
	t.Logf("zero-delay withhold verdict in %s (evidence deadline 3.5s untouched)", elapsed)
}

// TestZeroDelayWithholdOnUnterminatedFinalItemDone 锁定 EOF 补齐路径：上游
// 截断/异常结束时，最后的降智 item.done 行可能没有换行符——它在
// finishQualityPeek 的 flush 里才被解析。推理阶段已闭合却零增量不是
// "空证据"（空流走 15m 空闲冷却），必须按扣留路径（12h+归因）处理。
func TestZeroDelayWithholdOnUnterminatedFinalItemDone(t *testing.T) {
	t.Parallel()
	ciphertext := `"encrypted_content":"` + strings.Repeat("A", 8*1024) + `"`
	degraded := strings.Join([]string{
		"data: " + `{"type":"response.created","response":{"id":"resp_deg"}}`,
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","` + ciphertext[1:] + `}}`,
	}, "\n")
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(degraded)), qualityProtocolResponses, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("unterminated final item.done verdict = %s, want withhold", verdict)
	}
}
