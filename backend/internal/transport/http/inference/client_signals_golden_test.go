package inference

import (
	"encoding/json"
	"fmt"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func g31SignalCorpus() ([]http.Header, []string) {
	headers := []http.Header{nil,
		{"X-Claude-Code-Session-Id": {" main "}},
		{"X-Claude-Code-Session-Id": {"main"}, "X-Claude-Code-Agent-Id": {"sub"}},
		{"X-Claude-Code-Session-Id": {strings.Repeat("a", 1025)}},
		{"X-Claude-Code-Session-Id": {"main"}, "X-Claude-Code-Agent-Id": {strings.Repeat("a", 1025)}},
		{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"branch","window_id":"window"}`}, "X-Codex-Window-Id": {"coarse"}},
		{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":4,"window_id":"window"}`}, "X-Codex-Window-Id": {"coarse"}},
		{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":" ","window_id":"window"}`}},
		{"X-Codex-Turn-Metadata": {strings.Repeat(" ", 17<<10)}},
		{"X-Codex-Window-Id": {"window"}, "X-Session-Id": {"session"}},
		{"X-Session-Id": {"x"}, "Session-Id": {"plain"}, "Session_id": {"underscore"}},
		{"Session_id": {"underscore"}, "X-Conversation-Id": {"conv"}},
		{"X-Client-Session-Id": {"client"}, "X-Grok-Conv-Id": {"grok"}},
		{"X-Grok-Conv-Id": {"grok"}},
		{"X-Client-Request-Id": {"request-only"}},
	}
	bodies := []string{"", `{`, `null`, `{}`, `{"input":"hello"}`,
		`{"prompt_cache_key":" branch ","session_id":"coarse"}`,
		`{"prompt_cache_key":"","session_id":"coarse"}`,
		`{"prompt_cache_key":7,"session_id":"coarse"}`,
		`{"prompt_cache_key":"branch","metadata":7}`,
		`{"prompt_cache_key":` + fmt.Sprintf("%q", strings.Repeat("b", 1025)) + `,"session_id":"coarse"}`,
		`{"session_id":"snake","sessionId":"camel","conversation_id":"conv","conversationId":"cc"}`,
		`{"metadata":{"session_id":"nested","sessionId":"nested-camel","user_id":"abc_session_old"}}`,
		`{"metadata":{"user_id":"{\"session_id\":\"embedded\",\"sessionId\":\"camel\"}"}}`,
		`{"metadata":{"user_id":"{\"session_id\":7,\"sessionId\":\"camel\"}"}}`,
		`{"metadata":{"user_id":"prefix_session_first_session_last"}}`,
		`{"metadata":{"user_id":"ordinary-user"}}`,
		`{"client_metadata":{"x-codex-turn-metadata":{"prompt_cache_key":"branch","window_id":"window"},"x-codex-window-id":"other"}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"{\"window_id\":\"window\"}"}}`,
		`{"client_metadata":{"x-codex-turn-metadata":{"prompt_cache_key":3,"window_id":"window"},"x-codex-window-id":"other"}}`,
		`{"client_metadata":{"x-codex-window-id":12},"sessionId":"camel"}`,
		`{"system":"Generate a concise TITLE of this coding session","prompt_cache_key":"branch"}`,
		`{"system":[{"type":"text","text":"Generate a concise title of this coding session"}]}`,
		`{"system":[{"type":"text","text":"Generate a concise title of this coding session"}],"prompt_cache_key":7}`,
		`{"system":[{"type":"image","text":"Generate a concise title of this coding session"}],"prompt_cache_key":"branch"}`,
		`{"system":[{"type":"text","text":"Generate a concise title of this coding session"},{"type":"text","text":9}],"sessionId":"camel"}`,
		`{"prompt_cache_key":"你好:子会话","system":"ordinary system"}`,
	}
	return headers, bodies
}
func TestClientSignalsPreserveG30WirePrecedence(t *testing.T) {
	data, err := os.ReadFile("testdata/g31-signals-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err = json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	headers, bodies := g31SignalCorpus()
	got := map[string]string{}
	for i, h := range headers {
		for j, body := range bodies {
			seed := historydomain.ResolveClientSeed(extractClientSignals(h, []byte(body)))
			value := seed.Key
			if seed.PriorKey != "" {
				value = seed.PriorKey
				if seed.Key == seed.PriorKey {
					t.Fatal("named identity retained ambiguous key")
				}
			}
			got[fmt.Sprintf("header-%02d/body-%02d", i, j)] = value
		}
	}
	if !reflect.DeepEqual(got, want) {
		for key, value := range got {
			if value != want[key] {
				t.Errorf("%s seed=%q want=%q", key, value, want[key])
			}
		}
	}
}
func BenchmarkG31Signals(b *testing.B) {
	for _, claude := range []bool{false, true} {
		b.Run(fmt.Sprint(claude), func(b *testing.B) {
			var h http.Header
			if claude {
				h = http.Header{"X-Claude-Code-Session-Id": {"session"}}
			}
			body := []byte(`{"system":"rules","input":"` + strings.Repeat("hello world ", 10000) + `"}`)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = extractPromptCacheSeed(h, body)
			}
		})
	}
}
