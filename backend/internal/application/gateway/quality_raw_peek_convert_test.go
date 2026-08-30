package gateway

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestDeferredConversionPeeksRawThenConverts(t *testing.T) {
	t.Parallel()
	raw := strings.Join([]string{
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}",
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"step\"}",
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}",
		"",
	}, "\n\n")
	converted := false
	resp := &provider.Response{
		Body: io.NopCloser(strings.NewReader(raw)),
		ConvertStream: func(src io.ReadCloser) io.ReadCloser {
			converted = true
			return src
		},
	}
	proto := qualityProtocolForOperation(audit.OperationChat)
	if resp.ConvertStream != nil {
		proto = qualityProtocolResponses
	}
	if proto != qualityProtocolResponses {
		t.Fatalf("proto = %s, want responses for deferred conversion", proto)
	}
	replay, verdict, _, err := peekQualityStream(context.Background(), resp.Body, proto, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("raw summary delta must deliver, verdict=%s", verdict)
	}
	if converted {
		t.Fatal("conversion must not run during peek")
	}
	resp.Body = replay
	applyDeferredStreamConversion(resp)
	if !converted {
		t.Fatal("conversion must run at handoff")
	}
}
