package gateway

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// TestWithholdCleansUpConverterGoroutine：守卫扣留并关闭转换流后，转换器
// 的 streampipe goroutine 必须随 source 关闭而退出——网关侧的 replay Close
// 链（replayReadCloser → converter stream → source）此前只在泄漏测试里覆盖
// pump 一侧，转换器协程侧没有端到端验证。
func TestWithholdCleansUpConverterGoroutine(t *testing.T) {
	t.Parallel()
	nl := "\n\n"
	upstream := strings.Join([]string{
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"xx"}}`,
		"data: " + `{"type":"response.output_text.delta","delta":"bare"}`,
		"",
	}, nl)
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		converted := conversation.ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(upstream)), conversation.OperationChat, conversation.ResponseOptions{})
		replay, verdict, _, err := peekQualityStream(context.Background(), converted, qualityProtocolChat, QualityRetryRuntime{})
		if err != nil {
			t.Fatalf("peek err = %v", err)
		}
		if verdict != QualityWithhold {
			t.Fatalf("verdict = %s, want withhold", verdict)
		}
		_ = replay.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines did not settle after withhold closes: before=%d now=%d", before, runtime.NumGoroutine())
}
