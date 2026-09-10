package history

import (
	"encoding/json"
	"fmt"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestIdentityPreservesG30Keys(t *testing.T) {
	data, err := os.ReadFile("testdata/g31-identity-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err = json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	resolver := &IdentityResolver{}
	for _, name := range []string{"explicit", "soft", "assistant-a", "assistant-b", "unicode", "composer", "inherited", "empty"} {
		body := []byte(`{"instructions":"rules","input":"hello"}`)
		seed := ""
		model := "grok-4.6"
		switch name {
		case "explicit":
			seed = "session-1"
		case "assistant-a":
			body = []byte(`{"instructions":"rules","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"a"}]}`)
		case "assistant-b":
			body = []byte(`{"instructions":"rules","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"b"}]}`)
		case "unicode":
			body = []byte(`{"system":"规则：保持","messages":[{"role":"user","content":"测试你好"}]}`)
		case "composer":
			model = "grok-composer-2.5-fast"
		case "empty":
			body = nil
		}
		prior := Identity{}
		if name == "inherited" {
			prior.UpstreamID = "saved-hint"
		}
		request := NewIdentityRequest(7, historydomain.ClientSignals{PromptCacheKey: seed}, "", "request-1", "request-1", body)
		id := resolver.Resolve(request, IdentityTarget{Provider: "grok_build", Model: model, IsolatedWithoutSession: name == "composer"}, prior)
		got := map[string]any{"upstream": id.UpstreamID, "affinity": id.AffinityKey, "replay": id.ReplayKey, "soft": id.Soft, "isolated": id.Isolated}
		if !reflect.DeepEqual(got, want[name]) {
			t.Errorf("%s identity changed: got=%v want=%v", name, got, want[name])
		}
	}
}
func TestIdentityResolverOwnsConcurrentForksAndIndependentInstances(t *testing.T) {
	var owner, replica IdentityResolver
	target := IdentityTarget{Provider: "grok_build", Model: "grok-4.6"}
	request := func(reply string) *IdentityRequest {
		return NewIdentityRequest(7, historydomain.ClientSignals{}, "", "request", "request", []byte(fmt.Sprintf(`{"input":[{"role":"user","content":"opening"},{"role":"assistant","content":%q}]}`, reply)))
	}
	first := owner.Resolve(request("a"), target, Identity{})
	fork := owner.Resolve(request("b"), target, Identity{})
	if first.UpstreamID == fork.UpstreamID || first.ReplayKey != "" || fork.ReplayKey != "" {
		t.Fatal("fork or replay eligibility changed")
	}
	// A second instance starts independently, including when the fork arrives first.
	secondFirst := replica.Resolve(request("b"), target, Identity{})
	if secondFirst.UpstreamID != first.UpstreamID {
		t.Fatal("soft table leaked across instances")
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reply := "a"
			want := first
			if i%2 == 1 {
				reply = "b"
				want = fork
			}
			for j := 0; j < 10; j++ {
				if got := owner.Resolve(request(reply), target, Identity{}); got != want {
					t.Errorf("concurrent fork drifted: %v", got)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	explicit := NewIdentityRequest(7, historydomain.ClientSignals{PromptCacheKey: "session"}, "", "request", "request", nil)
	if a, b := owner.Resolve(explicit, target, Identity{}), replica.Resolve(explicit, target, Identity{}); a != b || a.ReplayKey == "" {
		t.Fatal("durable identity depends on instance table")
	}
}
func TestSoftConversationCapacityRetainsActiveHints(t *testing.T) {
	var reg softConversationRegistry
	now := time.Now()
	reg.entries = make(map[string]*softConversationEntry)
	reg.lastSweep = now
	for i := 0; i < softConversationRegistryCap; i++ {
		reg.entries[fmt.Sprint(i)] = &softConversationEntry{convID: fmt.Sprint(i), lastSeen: now.Add(time.Duration(i) * time.Nanosecond)}
	}
	if got := reg.resolveSoftConversation("32767", "32767", "base", "fork"); got != "32767" {
		t.Fatal("newest hint evicted")
	}
	if _, ok := reg.entries["0"]; ok {
		t.Fatal("oldest hint retained")
	}
	if n := len(reg.entries); n != softConversationRegistryCap*3/4 {
		t.Fatalf("eviction count=%d", n)
	}
	for i := 0; i < softConversationRegistryCap/4+4; i++ {
		reg.resolveSoftConversation(fmt.Sprint("open", i), fmt.Sprint("reply", i), "base", "fork")
		if n := len(reg.entries); n > softConversationRegistryCap+1 {
			t.Fatalf("unbounded indexes=%d", n)
		}
	}
}
func BenchmarkG31RegistryCapacity(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		r := newSoftConversationRegistry()
		now := time.Now()
		for j := 0; j < softConversationRegistryCap; j++ {
			r.entries[fmt.Sprint(j)] = &softConversationEntry{convID: fmt.Sprint(j), lastSeen: now.Add(time.Duration(j) * time.Nanosecond)}
		}
		b.StartTimer()
		r.resolveSoftConversation("new", "new", "new", "fork")
	}
}
func BenchmarkG31Identity(b *testing.B) {
	for _, kind := range []string{"explicit", "soft"} {
		b.Run(kind, func(b *testing.B) {
			body := benchAnchorBody()
			seed := ""
			if kind == "explicit" {
				seed = "session"
			}
			var resolver IdentityResolver
			signals := historydomain.ClientSignals{PromptCacheKey: seed}
			target := IdentityTarget{Provider: "grok_build", Model: "grok-4.6"}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := NewIdentityRequest(7, signals, "", "request-1", "request-1", body)
				_ = r.RouteSeed()
				_ = resolver.Resolve(r, target, Identity{})
				_ = resolver.Resolve(r, target, Identity{})
			}
		})
	}
}

func TestNamedIdentityCannotAliasAnOldRawEncoding(t *testing.T) {
	var resolver IdentityResolver
	signals := historydomain.ClientSignals{ClaudeSession: "a:agent:b", ClaudeAgent: "c"}
	seed := historydomain.ResolveClientSeed(signals)
	target := IdentityTarget{Provider: "grok_build", Model: "grok-4.5"}
	request := NewIdentityRequest(7, signals, "", "request", "request", nil)
	identity := resolver.Resolve(request, target, Identity{})
	// These are exactly the old v5 raw-key encodings, prior to G32's escaping.
	oldReplay := hexDigest(fmt.Sprintf("grok2api:build-replay:v5:7:grok_build:grok-4.5:%s", seed.Key))
	oldHint := digestUUID(fmt.Sprintf("grok2api:build-session:v5:7:grok_build:grok-4.5:%s", seed.Key))
	if identity.ReplayKey == oldReplay || identity.UpstreamID == oldHint {
		t.Fatal("new namespace aliases pre-upgrade raw identity")
	}
	expectedPrior := hexDigest("grok2api:build-replay:v5:7:grok_build:grok-4.5:claude:a:agent:b:agent:c")
	if identity.PriorReplayKey != expectedPrior {
		t.Fatal("old scope lookup changed bytes")
	}
	raw := resolver.Resolve(NewIdentityRequest(7, historydomain.ClientSignals{PromptCacheKey: seed.Key}, "", "request", "request", nil), target, Identity{})
	if raw.ReplayKey == identity.ReplayKey || raw.UpstreamID == identity.UpstreamID {
		t.Fatal("current raw identity impersonates named identity")
	}
	inherited := Identity{UpstreamID: "verified-old-hint", ReplayKey: expectedPrior}
	if got := resolver.Resolve(request, target, inherited); got != inherited {
		t.Fatalf("verified previous response was rekeyed: %+v", got)
	}
}
