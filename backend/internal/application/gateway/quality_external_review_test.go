package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestOversizedLineWithEOFDelivers：超长行与 EOF 同批到达时走 fail-open
// 而非空流（外部复核 9：空流短路曾覆盖 oversized 证据）。
func TestOversizedLineWithEOFDelivers(t *testing.T) {
	t.Parallel()
	huge := "data: " + strings.Repeat("x", 1<<21)
	replay, verdict, _, _, err := peekQualityStream(context.Background(),
		io.NopCloser(strings.NewReader(huge)), qualityProtocolChat,
		QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("verdict = %s, want deliver (oversized fail-open must win over empty-stream)", verdict)
	}
}
