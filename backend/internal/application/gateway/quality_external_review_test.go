package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestHugeGarbageLineNeverDelivers：>1MiB 的无法分类超长行（换行终止与
// EOF 同批两种形态）都不得放行——首尾分类提取不到任何思考证据时按空流
// 路径收口（可重试），绝不 fail-open。历史上该行为在两次外部复核间翻转
// 过，两条孪生用例（fails-open/delivers 之名）随之互相矛盾，现合并为
// 这一条语义诚实的锁定。
func TestHugeGarbageLineNeverDelivers(t *testing.T) {
	t.Parallel()
	for name, tail := range map[string]string{
		"newline-terminated": "\n\n",
		"eof-flushed":        "",
	} {
		huge := "data: " + strings.Repeat("x", 1<<21) + tail
		replay, verdict, _, err := peekQualityStream(context.Background(),
			io.NopCloser(strings.NewReader(huge)), qualityProtocolChat,
			QualityRetryRuntime{})
		if replay != nil {
			_ = replay.Close()
		}
		if verdict == QualityDeliver && err == nil {
			t.Fatalf("%s: verdict = %s, huge garbage must not fail-open", name, verdict)
		}
	}
}
