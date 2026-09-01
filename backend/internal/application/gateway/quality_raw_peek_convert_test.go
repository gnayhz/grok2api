package gateway

import (
	"context"
	"io"
	"net/http"
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
	proto := qualityPeekProtocol(audit.OperationChat, resp)
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

// 密文降智：peek 看 RAW Responses 即扣留。thinkingStart 只在 ConvertStream
// 里对 item.added 开空 thinking 块；生产 fail_closed 在 handoff 前关 body，
// 转换不得跑。
func TestRawPeekWithholdsCiphertextBeforeConvert(t *testing.T) {
	t.Parallel()
	raw := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"EqoBCkgIYj"}}`,
		`data: {"type":"response.output_text.delta","delta":"bare answer"}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
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
	proto := qualityPeekProtocol(audit.OperationMessages, resp)
	if proto != qualityProtocolResponses {
		t.Fatalf("proto = %s, want responses for deferred conversion", proto)
	}
	replay, verdict, _, err := peekQualityStream(context.Background(), resp.Body, proto, QualityRetryRuntime{})
	if replay != nil {
		_ = replay.Close()
	}
	if err != nil || verdict != QualityWithhold {
		t.Fatalf("raw ciphertext peek = %s err=%v, want withhold", verdict, err)
	}
	if converted {
		t.Fatal("conversion must not run during peek withhold (thinkingStart never sees item.added)")
	}
}

func TestDeferredJSONConversionPeeksRawThenConverts(t *testing.T) {
	t.Parallel()
	raw := "{\"id\":\"r1\",\"output\":[{\"type\":\"reasoning\",\"summary\":[{\"text\":\"step\"}]},{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"3\"}]}]}"
	converted := false
	resp := &provider.Response{
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(raw)),
		ConvertJSON: func(src []byte) ([]byte, error) {
			converted = true
			return []byte("{\"choices\":[{\"message\":{\"content\":\"3\",\"reasoning_content\":\"step\"}}]}"), nil
		},
	}
	proto := qualityPeekProtocol(audit.OperationChat, resp)
	if proto != qualityProtocolResponses {
		t.Fatalf("proto = %s, want responses for deferred JSON conversion", proto)
	}
	replay, verdict, _, err := peekQualityBody(resp.Body, QualityRetryRuntime{})
	if err != nil {
		t.Fatalf("peek err = %v", err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("raw reasoning summary must deliver, verdict=%s", verdict)
	}
	if converted {
		t.Fatal("JSON conversion must not run during peek")
	}
	resp.Body = replay
	applyDeferredStreamConversion(resp)
	if !converted {
		t.Fatal("JSON conversion must run at handoff")
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(out), "reasoning_content") {
		t.Fatalf("converted body = %s err=%v", out, err)
	}
}
