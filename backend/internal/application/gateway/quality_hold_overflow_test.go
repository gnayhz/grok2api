package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestPeekHoldBufferOverflowWithholds: >4MiB held buffer without thinking
// evidence must withhold (multi-megabyte accumulation); previously zero coverage.
func TestPeekHoldBufferOverflowWithholds(t *testing.T) {
	t.Parallel()
	chunk := "data: " + `{"type":"response.output_text.delta","delta":"wwww"}` + "\n\n"
	payload := strings.Repeat(chunk, (qualityHoldMaxBufferBytes/len(chunk))+8)
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(payload)), qualityProtocolResponses, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("overflow without thinking verdict = %s, want withhold", verdict)
	}
}
