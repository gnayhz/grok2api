package conversation

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestConvertedStreamRequiresTerminalEvent(t *testing.T) {
	for _, operation := range []string{OperationChat, OperationMessages} {
		for _, body := range []string{
			"",
			": heartbeat\n\n",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
			"data: [DONE]\n\n",
		} {
			t.Run(operation+"/"+body, func(t *testing.T) {
				stream := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(body)), operation, ResponseOptions{})
				defer stream.Close()
				data, err := io.ReadAll(stream)
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("missing completion error = %v, body = %s", err, data)
				}
				for _, marker := range []string{"[DONE]", `"finish_reason":"stop"`, "message_stop", `"stop_reason":"end_turn"`} {
					if strings.Contains(string(data), marker) {
						t.Fatalf("truncated stream manufactured success %q: %s", marker, data)
					}
				}
				if strings.Contains(body, "partial") && !strings.Contains(string(data), "partial") {
					t.Fatalf("observed partial text lost: %s", data)
				}
			})
		}
		for _, terminal := range []string{"response.completed", "response.incomplete", "response.failed", "error"} {
			t.Run(operation+"/"+terminal, func(t *testing.T) {
				body := "data: {\"type\":\"" + terminal + "\",\"response\":{\"status\":\"completed\"},\"error\":{\"message\":\"failure\"}}\n\n"
				stream := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(body)), operation, ResponseOptions{})
				defer stream.Close()
				if _, err := io.ReadAll(stream); err != nil {
					t.Fatalf("explicit terminal returned transport error: %v", err)
				}
			})
		}
	}
}
