package web

import (
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
)

func TestWebCompletionNeedsOutputBeyondReasoning(t *testing.T) {
	var parsed parsedChat
	parsed.Reasoning.WriteString("plan")
	if !errors.Is(checkChatOutput(&parsed), responsecheck.ErrEmptyOutput) {
		t.Fatal("reasoning-only accepted")
	}
	parsed.appendText("answer")
	if err := checkChatOutput(&parsed); err != nil {
		t.Fatal(err)
	}
	parsed.resetText("")
	parsed.ToolCalls = []parsedToolCall{{ID: "call_1", Name: "clock", Arguments: "{}"}}
	if err := checkChatOutput(&parsed); err != nil {
		t.Fatal(err)
	}
}

type completionFailWriter struct{ writes int }

func (w *completionFailWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("client closed")
}

func TestWebCompletionStopsAtFirstWriteFailure(t *testing.T) {
	for _, operation := range []string{"chat", "messages", "responses"} {
		writer := new(completionFailWriter)
		if err := writeStreamDone(writer, operation, "resp_1", "grok", parsedChat{}, map[string]any{}); err == nil {
			t.Fatalf("%s swallowed failure", operation)
		}
		if writer.writes != 1 {
			t.Fatalf("%s wrote after failure: %d", operation, writer.writes)
		}
	}
}

func TestForcedToolChoiceCannotCompleteWithProse(t *testing.T) {
	var parsed parsedChat
	parsed.appendText("I would call lookup")
	config := toolConfiguration{Choice: "required", ForcedName: "lookup"}
	if checkToolChoice(&parsed, config) == nil {
		t.Fatal("forced tool ignored")
	}
	parsed.ToolCalls = []parsedToolCall{{Name: "other"}}
	if checkToolChoice(&parsed, config) == nil {
		t.Fatal("wrong tool accepted")
	}
	parsed.ToolCalls = []parsedToolCall{{Name: "lookup"}}
	if err := checkToolChoice(&parsed, config); err != nil {
		t.Fatal(err)
	}
	if err := checkToolChoice(&parsed, toolConfiguration{Choice: "auto"}); err != nil {
		t.Fatal(err)
	}
}
