package history

import (
	"fmt"
	"strings"
	"testing"
)

func TestClientIdentityEncodingSeparatesNamespacesAndFieldBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		signals     ClientSignals
		want, prior string
	}{
		{"ordinary", ClientSignals{PromptCacheKey: " branch "}, "branch", ""},
		{"unicode raw", ClientSignals{PromptCacheKey: "你好:分支"}, "你好:分支", ""},
		{"claude first", ClientSignals{ClaudeSession: "a:agent:b", ClaudeAgent: "c"}, "g2a:identity:1:claude:9:a:agent:b:1:c", "claude:a:agent:b:agent:c"},
		{"claude second", ClientSignals{ClaudeSession: "a", ClaudeAgent: "b:agent:c"}, "g2a:identity:1:claude:1:a:9:b:agent:c", "claude:a:agent:b:agent:c"},
		{"claude raw", ClientSignals{PromptCacheKey: "claude:a:agent:b:agent:c"}, "g2a:identity:1:raw:24:claude:a:agent:b:agent:c", "claude:a:agent:b:agent:c"},
		{"window", ClientSignals{CodexWindow: "x"}, "g2a:identity:1:window:1:x", "codex:window:x"},
		{"window raw", ClientSignals{PromptCacheKey: "codex:window:x"}, "g2a:identity:1:raw:14:codex:window:x", "codex:window:x"},
		{"title", ClientSignals{PromptCacheKey: "x", ClaudeSession: "c", SystemTexts: []string{"Generate a concise title of this coding session"}}, "g2a:identity:1:title:1:x", "aux:claude-title:x"},
		{"title raw", ClientSignals{PromptCacheKey: "aux:claude-title:x"}, "g2a:identity:1:raw:18:aux:claude-title:x", "aux:claude-title:x"},
		{"new prefix raw", ClientSignals{PromptCacheKey: "g2a:identity:1:window:1:x"}, "g2a:identity:1:raw:25:g2a:identity:1:window:1:x", "g2a:identity:1:window:1:x"},
	}
	seen := map[string]string{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ResolveClientSeed(test.signals)
			if got.Key != test.want || got.PriorKey != test.prior {
				t.Fatalf("identity=%+v want key=%q prior=%q", got, test.want, test.prior)
			}
			if prior, ok := seen[got.Key]; ok {
				t.Fatalf("collides with %s", prior)
			}
			seen[got.Key] = test.name
		})
	}
	// A tuple corpus includes separator-bearing, empty/default, Unicode and
	// maximum-length fields. Equivalent defaults intentionally share identity.
	fields := []string{"a", "b", ":", "agent:", ":agent:", "a:agent:b", "你好", strings.Repeat("x", MaxClientSeedBytes)}
	pairs := map[string]string{}
	for _, session := range fields {
		for _, agent := range fields {
			seed := ResolveClientSeed(ClientSignals{ClaudeSession: session, ClaudeAgent: agent})
			pair := fmt.Sprintf("%q/%q", session, agent)
			if old, ok := pairs[seed.Key]; ok {
				t.Fatalf("%s collides with %s", pair, old)
			}
			pairs[seed.Key] = pair
			if raw := ResolveClientSeed(ClientSignals{PromptCacheKey: seed.Key}); raw.Key == seed.Key {
				t.Fatal("raw impersonated encoded Claude identity")
			}
		}
	}
}
func TestClientIdentitySignalMigrationAndBounds(t *testing.T) {
	for _, key := range []string{"plain", "claude:a:agent:main", "codex:window:x", "g2a:identity:1:raw:1:x"} {
		want := ResolveClientSeed(ClientSignals{PromptCacheKey: key})
		for _, signals := range []ClientSignals{{CodexTurn: TurnSignals{PromptCacheKey: key}}, {ClientTurn: TurnSignals{PromptCacheKey: key}}, {XSession: key}, {MetadataSession: key}, {BodySession: key}} {
			if got := ResolveClientSeed(signals); got != want {
				t.Fatalf("raw source changed identity for %q: %+v", key, got)
			}
		}
	}
	first := ResolveClientSeed(ClientSignals{ClaudeSession: "a"})
	if got := ResolveClientSeed(ClientSignals{ClaudeUser: UserSessionSignals{Suffix: "a"}, ClaudeAgent: " main "}); got != first {
		t.Fatal("Claude metadata/default changed identity")
	}
	if got := ResolveClientSeed(ClientSignals{ClaudeSession: strings.Repeat("x", 1025), XSession: "fallback"}); got.Key != "fallback" {
		t.Fatal("oversized field did not fall back")
	}
	if got := RawClientSeed(strings.Repeat("x", 1025)); got.Key != "" {
		t.Fatal("oversized fallback escaped limit")
	}
}
