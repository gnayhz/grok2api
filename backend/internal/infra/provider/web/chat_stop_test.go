package web

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestWebChatStopInJSONAndEveryStreamBoundary(t *testing.T) {
	options := conversation.ResponseOptions{StopSequences: []string{" STOP"}}
	var parsed parsedChat
	parsed.appendText("answer STOP hidden")
	result := buildOpenAIResult("chat", "resp_stop", "grok", parsed, false, options)
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "STOP") || strings.Contains(string(raw), "hidden") {
		t.Fatalf("JSON stop ignored: %s", raw)
	}
	for _, text := range []string{"answer STOP hidden", "answer ST"} {
		for size := 1; size <= len(text); size++ {
			var source strings.Builder
			for start := 0; start < len(text); start += size {
				data, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{"token": text[start:min(start+size, len(text))], "messageTag": "final"}}})
				source.Write(data)
			}
			a := new(Adapter)
			stream := a.streamOpenAIResponse(context.Background(), io.NopCloser(strings.NewReader(source.String())), new(egress.Lease), account.Credential{}, "resp_stop", "grok", "chat", "prompt", nil, toolConfiguration{}, true, options, nil, "")
			data, err := io.ReadAll(stream)
			stream.Close()
			if err != nil {
				t.Fatal(err)
			}
			var visible strings.Builder
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var item struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &item); err != nil {
					t.Fatal(err)
				}
				for _, choice := range item.Choices {
					visible.WriteString(choice.Delta.Content)
				}
			}
			want := "answer"
			if text == "answer ST" {
				want = text
			}
			if visible.String() != want || !strings.Contains(string(data), "data: [DONE]") {
				t.Fatalf("size=%d got=%q want=%q", size, visible.String(), want)
			}
		}
	}
}
