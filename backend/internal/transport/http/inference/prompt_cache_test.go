package inference

import (
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"net/http"
	"strings"
	"testing"
)

func TestExtractPromptCacheSeedSupportsClaudeCodeForms(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		body    string
		want    string
	}{
		{name: "claude header", headers: http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Session-Id": {"generic-session"}}, body: `{"metadata":{"session_id":"body-session"}}`, want: "g2a:identity:1:claude:14:claude-session:4:main"},
		{name: "claude agent header", headers: http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Claude-Code-Agent-Id": {"agent-7"}}, want: "g2a:identity:1:claude:14:claude-session:7:agent-7"},
		{name: "codex turn prompt cache", headers: http.Header{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"codex-cache","window_id":"window-1"}`}}, want: "codex-cache"},
		{name: "codex turn window", headers: http.Header{"X-Codex-Turn-Metadata": {`{"window_id":"window-1"}`}}, want: "g2a:identity:1:window:8:window-1"},
		{name: "codex window header", headers: http.Header{"X-Codex-Window-Id": {"window-2"}}, want: "g2a:identity:1:window:8:window-2"},
		{name: "generic header", headers: http.Header{"X-Session-Id": {"generic-session"}}, want: "generic-session"},
		{name: "codex hyphen header", headers: http.Header{"Session-Id": {"codex-session"}}, want: "codex-session"},
		{name: "underscore session header", headers: http.Header{"Session_id": {"underscore-session"}}, want: "underscore-session"},
		{name: "metadata snake case", body: `{"metadata":{"session_id":"snake-session"}}`, want: "snake-session"},
		{name: "metadata camel case", body: `{"metadata":{"sessionId":"camel-session"}}`, want: "camel-session"},
		{name: "embedded json user id", body: `{"metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"embedded-session\"}"}}`, want: "g2a:identity:1:claude:16:embedded-session:4:main"},
		{name: "suffix user id", body: `{"metadata":{"user_id":"user_account_session_123e4567-e89b-12d3-a456-426614174000"}}`, want: "g2a:identity:1:claude:36:123e4567-e89b-12d3-a456-426614174000:4:main"},
		{name: "conversation snake case", body: `{"conversation_id":"conversation-session"}`, want: "conversation-session"},
		{name: "conversation camel case", body: `{"conversationId":"camel-conversation"}`, want: "camel-conversation"},
		{name: "body prompt_cache_key", body: `{"prompt_cache_key":"client-session"}`, want: "client-session"},
		{name: "body cache key overrides shared window", headers: http.Header{"X-Codex-Window-Id": {"shared-window"}}, body: `{"prompt_cache_key":"main-branch"}`, want: "main-branch"},
		{name: "auxiliary body key keeps its branch", headers: http.Header{"X-Codex-Window-Id": {"shared-window"}}, body: `{"prompt_cache_key":"aux-branch"}`, want: "aux-branch"},
		{name: "body key overrides stale turn metadata", headers: http.Header{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"stale-key","window_id":"shared-window"}`}}, body: `{"prompt_cache_key":"current-branch"}`, want: "current-branch"},
		{name: "body key overrides generic session", headers: http.Header{"Session-Id": {"shared-session"}}, body: `{"prompt_cache_key":"specific-branch"}`, want: "specific-branch"},
		{name: "empty body key retains header fallback", headers: http.Header{"X-Codex-Window-Id": {"shared-window"}}, body: `{"prompt_cache_key":" "}`, want: "g2a:identity:1:window:13:shared-window"},
		{name: "invalid body retains header fallback", headers: http.Header{"X-Codex-Window-Id": {"shared-window"}}, body: `{`, want: "g2a:identity:1:window:13:shared-window"},
		{name: "body codex turn metadata", body: `{"client_metadata":{"x-codex-turn-metadata":"{\"prompt_cache_key\":\"body-codex-cache\"}"}}`, want: "body-codex-cache"},
		{name: "body codex turn metadata object", body: `{"client_metadata":{"x-codex-turn-metadata":{"prompt_cache_key":"body-codex-object"}}}`, want: "body-codex-object"},
		{name: "body codex window", body: `{"client_metadata":{"x-codex-window-id":"body-window"}}`, want: "g2a:identity:1:window:11:body-window"},
		// http.Header canonicalizes "session_id" to Session-Id.
		{name: "compatible client session_id via Set", headers: func() http.Header { h := make(http.Header); h.Set("session_id", "client-header-session"); return h }(), want: "client-header-session"},
		{name: "grok conv header", headers: http.Header{"X-Grok-Conv-Id": {"grok-conv-session"}}, want: "grok-conv-session"},
		{name: "per request id ignored", headers: http.Header{"X-Client-Request-Id": {"request-123"}}, want: ""},
		{name: "ordinary user id", body: `{"metadata":{"user_id":"user-123"}}`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := extractPromptCacheSeed(test.headers, []byte(test.body)); got != test.want {
				t.Fatalf("seed = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExtractPromptCacheSeedIsolatesClaudeCodeTitleRequest(t *testing.T) {
	headers := http.Header{"X-Claude-Code-Session-Id": {"claude-session"}}
	body := []byte(`{
		"system":[
			{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.170; cch=abcde;"},
			{"type":"text","text":"Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session."}
		],
		"messages":[{"role":"user","content":"<session>hello</session>"}]
	}`)
	if seed := extractPromptCacheSeed(headers, body); seed != "" {
		t.Fatalf("title request seed = %q, want empty explicit seed", seed)
	}
}

func TestExtractPromptCacheSeedRejectsOversizedValues(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-Claude-Code-Session-Id", strings.Repeat("x", historydomain.MaxClientSeedBytes+1))
	if seed := extractPromptCacheSeed(headers, nil); seed != "" {
		t.Fatalf("oversized seed = %q", seed)
	}
}

func TestPromptCacheKeySurvivesWindowChangesAndSignalMigration(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"main-cache"}`)
	for _, headers := range []http.Header{
		nil,
		{"X-Codex-Window-Id": {"window-before"}},
		{"X-Codex-Window-Id": {"window-after"}},
		{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"main-cache","window_id":"window-after"}`}},
	} {
		if got := extractPromptCacheSeed(headers, body); got != "main-cache" {
			t.Fatalf("cache identity drifted: %q", got)
		}
	}
	if got := extractPromptCacheSeed(http.Header{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"main-cache"}`}}, nil); got != "main-cache" {
		t.Fatalf("metadata-only identity drifted: %q", got)
	}
}

func TestClaudeTitleWithExplicitCacheKeyCannotShareMainReplay(t *testing.T) {
	headers := http.Header{"X-Claude-Code-Session-Id": {"same-session"}}
	main := []byte(`{"prompt_cache_key":"same-cache","messages":[{"role":"user","content":"main"}]}`)
	title := []byte(`{"prompt_cache_key":"same-cache","system":[{"type":"text","text":"Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session."}],"messages":[{"role":"user","content":"main"}]}`)
	mainSeed, titleSeed := extractPromptCacheSeed(headers, main), extractPromptCacheSeed(headers, title)
	if mainSeed == titleSeed || titleSeed == "" {
		t.Fatalf("title and main must be isolated: main=%q title=%q", mainSeed, titleSeed)
	}
	if got := extractPromptCacheSeed(headers, main); got != mainSeed {
		t.Fatal("title changed main identity")
	}
}

func extractPromptCacheSeed(headers http.Header, body []byte) string {
	return historydomain.ResolveClientSeed(extractClientSignals(headers, body)).Key
}
