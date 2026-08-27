package conversation

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestHugeFailedEmitsErrorNotNormalFinish(t *testing.T) {
	t.Parallel()
	cipher := strings.Repeat("F", maxParsedSSEJSONBytes+32)
	stream := strings.Join([]string{
		"event: response.created",
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.6\"}}",
		"",
		"event: response.failed",
		"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"},\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}]}}",
		"",
		"",
	}, string([]byte{10}))
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, "\"finish_reason\":\"stop\"") {
		t.Fatalf("huge failed converted as normal stop: %s", text)
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	found := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if json.Unmarshal([]byte(body), &payload) != nil {
			continue
		}
		if payload.Error.Message != "" || payload.Error.Code != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error event: %s", text)
	}
	if payload.Error.Code != "server_error" || payload.Error.Message != "boom" {
		t.Fatalf("error not extracted from truncated failed frame: code=%q message=%q body=%s", payload.Error.Code, payload.Error.Message, text)
	}
}
