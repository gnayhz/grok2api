package conversation

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseResponsePreservesFragmentOrderAndReasoningFallback(t *testing.T) {
	value := responseEnvelope{Output: []responseItem{
		{Type: "message", Content: []responseContent{{Type: "output_text", Text: "hello "}, {Type: "refusal", Refusal: "cannot "}, {Type: "output_text", Text: "世界"}}},
		{Type: "reasoning", Content: []responseContent{{Type: "reasoning_text", Text: "raw "}, {Type: "reasoning_text", Text: "thought"}}, Summary: []responseContent{{Text: "ignored"}}},
		{Type: "reasoning", Content: []responseContent{{Type: "reasoning_text", Text: ""}}, Summary: []responseContent{{Text: " + "}, {Text: "summary"}}},
		{Type: "message", Content: []responseContent{{Type: "refusal", Refusal: "comply"}, {Type: "output_text", Text: "!"}}},
	}}
	parsed := parseResponse(value)
	if parsed.Text != "hello 世界!" || parsed.Reasoning != "raw thought + summary" || parsed.Refusal != "cannot comply" {
		t.Fatalf("fragment aggregation changed: %+v", parsed)
	}
}

func BenchmarkParseResponseFragments(b *testing.B) {
	for _, count := range []int{128, 2048} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			value := responseEnvelope{CreatedAt: 123, Output: []responseItem{{Type: "message"}, {Type: "reasoning"}}}
			fragment := strings.Repeat("x", 16)
			for range count {
				value.Output[0].Content = append(value.Output[0].Content, responseContent{Type: "output_text", Text: fragment})
				value.Output[1].Content = append(value.Output[1].Content, responseContent{Type: "reasoning_text", Text: fragment})
			}
			b.ReportAllocs()
			b.SetBytes(int64(count * len(fragment) * 2))
			for b.Loop() {
				parsed := parseResponse(value)
				if len(parsed.Text) != count*len(fragment) || len(parsed.Reasoning) != len(parsed.Text) {
					b.Fatal("lost fragments")
				}
			}
		})
	}
}
