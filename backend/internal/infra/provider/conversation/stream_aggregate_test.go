package conversation

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestAggregateTextRecoveredWithoutDuplicateDeltas(t *testing.T) {
	for _, operation := range []string{OperationChat, OperationMessages} {
		for _, padding := range []int{0, 80 << 10} {
			for _, mode := range []string{"completed_only", "item_done", "delta", "mixed"} {
				t.Run(fmt.Sprintf("%s/%d/%s", operation, padding, mode), func(t *testing.T) {
					message := `{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"aggregate"}]}`
					var frames strings.Builder
					if mode == "delta" || mode == "mixed" {
						frames.WriteString("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"aggregate\"}\n\n")
					}
					if mode == "item_done" || mode == "delta" {
						frames.WriteString(`data: {"type":"response.output_item.done","item":` + message + `,"padding":"` + strings.Repeat("x", padding) + `"}` + "\n\n")
					}
					output := message
					want := "aggregate"
					if mode == "mixed" {
						output += `,{"id":"msg_2","type":"message","content":[{"type":"output_text","text":"second"}]}`
						want += "second"
					}
					frames.WriteString(`data: {"type":"response.completed","response":{"status":"completed","output":[` + output + `]},"padding":"` + strings.Repeat("x", padding) + `"}` + "\n\n")
					body := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(frames.String())), operation, ResponseOptions{})
					defer body.Close()
					raw, err := io.ReadAll(body)
					if err != nil {
						t.Fatal(err)
					}
					var text strings.Builder
					for _, line := range strings.Split(string(raw), "\n") {
						if !strings.HasPrefix(line, "data:") {
							continue
						}
						var event struct {
							Choices []struct {
								Delta struct {
									Content string `json:"content"`
								} `json:"delta"`
							} `json:"choices"`
							Delta struct {
								Text string `json:"text"`
							} `json:"delta"`
						}
						if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &event) != nil {
							continue
						}
						for _, choice := range event.Choices {
							text.WriteString(choice.Delta.Content)
						}
						text.WriteString(event.Delta.Text)
					}
					if text.String() != want {
						t.Fatalf("text=%q want=%q", text.String(), want)
					}
				})
			}
		}
	}
}
