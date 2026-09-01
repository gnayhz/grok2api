package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

func TestInternSSETypeKnown(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"response.created",
		"response.reasoning_summary_text.delta",
		"response.output_text.delta",
		"content_block_delta",
		"message_stop",
	} {
		got := jsonpeek.InternType([]byte(want))
		if got != want {
			t.Fatalf("%q interned to %q", want, got)
		}
	}
	if jsonpeek.InternType(nil) != "" {
		t.Fatal("empty")
	}
	if jsonpeek.InternType([]byte("response.unknown_frame")) != "response.unknown_frame" {
		t.Fatal("unknown type must pass through")
	}
}

func TestInternSSETypeDrivesResponsesPeek(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, EvidenceTimeout: 2 * time.Second, CreatedTimeout: 2 * time.Second}
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1"}}`,
		`data: {"type":"response.in_progress"}`,
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"plan"}`,
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		`data: {"type":"response.completed","response":{"usage":{"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1}}}}`,
	}, "\n\n") + "\n\n"
	_, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolResponses, cfg)
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("verdict=%s err=%v", verdict, err)
	}
	if jsonpeek.RootStringField([]byte(`{"type":"response.created"}`), "type") != jsonpeek.InternType(jsonpeek.RootStringBytes([]byte(`{"type":"response.created"}`), "type")) {
		t.Fatal("RootStringField and interned RootStringBytes must agree on type")
	}
}
