package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRefusalEvidenceAgreesAcrossProtocolsAndDeliveryModes(t *testing.T) {
	for _, tc := range []struct{ protocol, stream, body string }{
		{qualityProtocolResponses, `{"type":"response.refusal.delta","delta":"cannot help"}`, `{"output":[{"type":"message","content":[{"type":"refusal","refusal":"cannot help"}]}]}`},
		{qualityProtocolChat, `{"choices":[{"index":0,"delta":{"refusal":"cannot help"}}]}`, `{"choices":[{"index":0,"message":{"refusal":"cannot help"}}]}`},
		{qualityProtocolAnthropic, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"cannot help"}}`, `{"content":[{"type":"text","text":"cannot help"}]}`},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			body, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader("data: "+tc.stream+"\n\n")), tc.protocol, QualityRetryRuntime{})
			_ = body.Close()
			if err != nil || verdict != QualityWithhold {
				t.Fatalf("stream verdict=%s err=%v", verdict, err)
			}
			body, verdict, _, err = peekQualityBody(io.NopCloser(strings.NewReader(tc.body)), QualityRetryRuntime{})
			_ = body.Close()
			if err != nil || verdict != QualityWithhold {
				t.Fatalf("JSON verdict=%s err=%v", verdict, err)
			}
		})
	}
}

func TestChoiceEvidenceCannotAuthorizeADifferentGeneration(t *testing.T) {
	for _, choices := range []string{
		`[{"index":0,"delta":{"reasoning_content":"plan"}},{"index":1,"delta":{"content":"bare"}}]`,
		`[{"index":1,"delta":{"reasoning_content":"plan"}}]`,
		`[{"index":"0","delta":{"reasoning_content":"plan"}}]`,
	} {
		for _, usage := range []string{"", `,"usage":{"completion_tokens":2}`} {
			payload := `{"choices":` + choices + usage + `}`
			body, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader("data: "+payload+"\n\n")), qualityProtocolChat, QualityRetryRuntime{})
			_ = body.Close()
			if verdict != QualityWait || !errors.Is(err, errQualityChoices) {
				t.Fatalf("choices=%s verdict=%s err=%v", choices, verdict, err)
			}
		}
	}
	jsonBody := `{"choices":[{"index":0,"message":{"reasoning_content":"plan"}},{"index":1,"message":{"content":"bare"}}]}`
	body, verdict, _, err := peekQualityBody(io.NopCloser(strings.NewReader(jsonBody)), QualityRetryRuntime{})
	_ = body.Close()
	if verdict != QualityWait || !errors.Is(err, errQualityChoices) {
		t.Fatalf("JSON verdict=%s err=%v", verdict, err)
	}
}

func TestMultipleChoiceRequestRejectedBeforeForward(t *testing.T) {
	for _, body := range []string{
		`{"model":"grok-4.6","n":2,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model": "grok-4.6", "messages": [{"role": "user", "content": "hi"}], "stream": false, "n": 2}`,
		`{"model":"grok-4.6","n":1,"n":2,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"grok-4.6","\u006e":2,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
			service, _ := newGuardLoopService(t, adapter, "multi-request")
			input := guardLoopInput("n-two", true)
			input.Body = []byte(body)
			result, err := service.CreateChatCompletion(context.Background(), input)
			if result != nil {
				_ = result.Body.Close()
				t.Fatal("multi-choice request was delivered")
			}
			var failure *UpstreamFailure
			if !errors.As(err, &failure) || failure.Code != "unsupported_choices" {
				t.Fatalf("request error=%v", err)
			}
		})
	}
}
