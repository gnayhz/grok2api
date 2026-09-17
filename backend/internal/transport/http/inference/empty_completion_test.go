package inference

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	"github.com/gin-gonic/gin"
)

func TestReasoningOnlyCompletionReturnsErrorBeforeSuccess(t *testing.T) {
	const frames = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_empty\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"thinking\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\"}],\"usage\":{\"output_tokens\":23,\"output_tokens_details\":{\"reasoning_tokens\":23}}}}\n\n"
	for _, tc := range []struct {
		operation string
		protocol  streamProtocol
		success   string
	}{
		{"responses", streamProtocolResponses, `"type":"response.completed"`},
		{"chat", streamProtocolChat, `"finish_reason":"stop"`},
		{"messages", streamProtocolAnthropic, "event: message_stop"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			var source io.ReadCloser = responsecheck.Stream(responseflow.New(io.NopCloser(strings.NewReader(frames)), nil))
			if tc.operation != "responses" {
				source = conversation.ConvertResponseStreamWithOptions(source, tc.operation, conversation.ResponseOptions{AnthropicThinking: true})
			}
			defer source.Close()
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			_, err := copyStreamWithCompletion(ctx.Writer, source, tc.protocol, nil, "", nil)
			if !errors.Is(err, responsecheck.ErrEmptyOutput) {
				t.Fatalf("error=%v", err)
			}
			body := recorder.Body.String()
			if strings.Contains(body, tc.success) {
				t.Fatalf("false success: %s", body)
			}
			if !strings.Contains(body, "上游已结束但未返回答案或工具输出") || !strings.Contains(body, "thinking") {
				t.Fatalf("missing explicit error or partial output: %s", body)
			}
		})
	}
}
