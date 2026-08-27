package gateway

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestOversizedEncryptedContentWithholds(t *testing.T) {
	t.Parallel()
	cipher := strings.Repeat("A", 1<<20+4096)
	body := fmt.Sprintf(
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n"+
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"%s\"}}\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"degraded answer with no visible thinking\"}\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":80,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n"+
			"data: [DONE]\n",
		cipher)
	replay, verdict, _, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolResponses,
		QualityRetryRuntime{MinOutputTokens: 8, HoldTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("verdict = %s, want withhold (ciphertext dump must not fail-open)", verdict)
	}
}

func TestOversizedReasoningDeltaStillDelivers(t *testing.T) {
	t.Parallel()
	delta := strings.Repeat("thought ", 1<<17)
	body := fmt.Sprintf(
		"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"%s\"}\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n"+
			"data: [DONE]\n",
		delta)
	replay, verdict, _, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolResponses,
		QualityRetryRuntime{MinOutputTokens: 8, HoldTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("verdict = %s, want deliver (oversized thinking delta is evidence)", verdict)
	}
}
