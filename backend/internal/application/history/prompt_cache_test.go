package history

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestResolveBuildSessionIdentityIsStableAndTenantIsolated(t *testing.T) {
	resolver := &IdentityResolver{}
	base := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", "request-1", nil)
	if len(base.UpstreamID) != 36 || len(base.AffinityKey) != 64 || len(base.ReplayKey) != 64 || base != resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", "request-1", nil) {
		t.Fatalf("unstable identity = %#v", base)
	}
	for name, value := range map[string]Identity{
		"client":   resolver.resolveTestIdentity(8, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", "request-1", nil),
		"provider": resolver.resolveTestIdentity(7, accountdomain.ProviderConsole, "grok-4.5", "", "session-1", "request-1", nil),
		"session":  resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-2", "request-1", nil),
	} {
		if value.UpstreamID == base.UpstreamID || value.AffinityKey == base.AffinityKey {
			t.Fatalf("%s was not Isolated: %#v vs %#v", name, value, base)
		}
	}
}

func TestEnsureBuildComposerSessionIdentityIsRequestStable(t *testing.T) {
	resolver := &IdentityResolver{}
	empty := Identity{}
	first := ensureTestComposerIdentity(empty, 7, accountdomain.ProviderBuild, modeldomain.GrokComposer25Fast, "request-1")
	repeated := ensureTestComposerIdentity(empty, 7, accountdomain.ProviderBuild, modeldomain.GrokComposer25Fast, "request-1")
	other := ensureTestComposerIdentity(empty, 7, accountdomain.ProviderBuild, modeldomain.GrokComposer25Fast, "request-2")
	if first.UpstreamID == "" || first.AffinityKey == "" || !first.Isolated || first.ReplayKey != "" || first != repeated {
		t.Fatalf("Composer isolated identity unstable: first=%#v repeated=%#v", first, repeated)
	}
	if first.UpstreamID == other.UpstreamID || first.AffinityKey == other.AffinityKey {
		t.Fatalf("different Composer requests shared identity: first=%#v other=%#v", first, other)
	}
	if got := ensureTestComposerIdentity(empty, 7, accountdomain.ProviderBuild, "grok-4.5", "request-1"); got != empty {
		t.Fatalf("non-Composer request gained an identity: %#v", got)
	}
	explicit := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, modeldomain.GrokComposer25Fast, "client-session", "", "request-1", nil)
	if got := ensureTestComposerIdentity(explicit, 7, accountdomain.ProviderBuild, modeldomain.GrokComposer25Fast, "request-1"); got != explicit {
		t.Fatalf("explicit Composer identity was overwritten: got=%#v want=%#v", got, explicit)
	}
	soft := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, modeldomain.GrokComposer25Fast, "", "", "request-1", []byte(`{"input":"same opening message"}`))
	isolated := ensureTestComposerIdentity(soft, 7, accountdomain.ProviderBuild, modeldomain.GrokComposer25Fast, "request-1")
	if !soft.Soft || isolated.UpstreamID == soft.UpstreamID || isolated.Soft || !isolated.Isolated {
		t.Fatalf("soft Composer identity was not Isolated: soft=%#v isolated=%#v", soft, isolated)
	}
	softNonComposer := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", []byte(`{"input":"same opening message"}`))
	if got := ensureTestComposerIdentity(softNonComposer, 7, accountdomain.ProviderBuild, "grok-4.5", "request-1"); got != softNonComposer {
		t.Fatalf("non-Composer soft identity was overwritten: got=%#v want=%#v", got, softNonComposer)
	}
}

func TestEnsureBuildComposerSessionIdentityPreservesEmptyInheritedReplayKey(t *testing.T) {
	// previous_response_id 继承 ownership：UpstreamID 非空、soft=false、ReplayKey 可能为空。
	// Composer 隔离不得给空键续写铸造 ReplayKey（否则 CaptureBody 会打开）。
	inherited := Identity{UpstreamID: "cache-from-ownership", ReplayKey: ""}
	got := ensureTestComposerIdentity(inherited, 7, accountdomain.ProviderBuild, "grok-4.6", "request-1")
	if got != inherited || got.ReplayKey != "" {
		t.Fatalf("non-Composer inherit mutated empty ReplayKey: %#v", got)
	}
	composer := ensureTestComposerIdentity(inherited, 7, accountdomain.ProviderBuild, modeldomain.GrokComposer25Fast, "request-1")
	if composer.UpstreamID != inherited.UpstreamID || composer.ReplayKey != "" || composer.Isolated {
		t.Fatalf("Composer inherit must keep empty ReplayKey: %#v", composer)
	}
}

func TestResolveBuildSessionIdentitySeparatesAffinityFromUpstreamSession(t *testing.T) {
	resolver := &IdentityResolver{}
	first := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", "request-1", nil)
	otherModel := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.3", "", "session-1", "request-1", nil)
	if first.UpstreamID == "" || first.UpstreamID == otherModel.UpstreamID || first.ReplayKey == otherModel.ReplayKey {
		t.Fatalf("model-specific upstream session was not Isolated: first=%#v other=%#v", first, otherModel)
	}
	if first.AffinityKey == otherModel.AffinityKey {
		t.Fatalf("model-specific account affinity was not Isolated: %#v", first)
	}
}

func TestResolveBuildSoftSessionIdentityIsModelScoped(t *testing.T) {
	resolver := &IdentityResolver{}
	body := []byte(`{"instructions":"stable system","input":[{"role":"user","content":"hello"}]}`)
	first := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", body)
	otherModel := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.3", "", "", "request-1", body)
	if first.UpstreamID == "" || first.AffinityKey == "" || !first.Soft || !otherModel.Soft {
		t.Fatalf("soft identity missing: first=%#v other=%#v", first, otherModel)
	}
	if first.UpstreamID == otherModel.UpstreamID || first.AffinityKey == otherModel.AffinityKey {
		t.Fatalf("soft identity must be model scoped: first=%#v other=%#v", first, otherModel)
	}
}

func TestResolveBuildSessionIdentityChatUserOnlyDoesNotEnableReplay(t *testing.T) {
	resolver := &IdentityResolver{}
	// 无 prompt_cache_key / session seed 的 chat.completions：软会话可有亲和，但 ReplayKey 必须空，CaptureBody 不会包装。
	body := []byte(`{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	got := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "", "request-1", body)
	if !got.Soft || got.UpstreamID == "" || got.AffinityKey == "" {
		t.Fatalf("expected soft chat identity, got %#v", got)
	}
	if got.ReplayKey != "" {
		t.Fatalf("chat without explicit session must not enable reasoning replay: %#v", got)
	}
	explicit := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.6", "", "session-1", "request-1", body)
	if explicit.Soft || explicit.ReplayKey == "" || explicit.ReplayKey == got.ReplayKey {
		t.Fatalf("explicit session must enable replay: soft=%#v explicit=%#v", got, explicit)
	}
}

func TestResolveBuildSessionIdentityPrefersSessionSignal(t *testing.T) {
	resolver := &IdentityResolver{}
	first := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "client-key", "session-1", "request-1", nil)
	second := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "client-key", "session-2", "request-1", nil)
	if first.UpstreamID == "" || first == second {
		t.Fatalf("session signal did not take precedence: first=%#v second=%#v", first, second)
	}
	clientKeyOnly := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "client-key", "", "request-1", nil)
	if clientKeyOnly.UpstreamID == "" || clientKeyOnly == first {
		t.Fatalf("explicit key fallback was not Isolated: session=%#v explicit=%#v", first, clientKeyOnly)
	}
	if value := resolver.resolveTestIdentity(0, accountdomain.ProviderBuild, "grok-4.5", "client-key", "", "request-1", nil); value != (Identity{}) {
		t.Fatal("identity without client ownership should be empty")
	}
	if value := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", nil); value != (Identity{}) {
		t.Fatal("identity without an explicit key or session or body should be empty")
	}
}

func TestResolveBuildSessionIdentitySoftFromMessagesIsStableAcrossTurns(t *testing.T) {
	resolver := &IdentityResolver{}
	turn1 := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello world"}]}`)
	turn2 := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello world"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`)
	first := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", turn1)
	second := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-2", turn2)
	if !first.Soft || first.UpstreamID == "" {
		t.Fatalf("expected soft identity, got %#v", first)
	}
	if first.ReplayKey != "" {
		t.Fatalf("soft identity must not enable reasoning replay: %#v", first)
	}
	// 第 2 轮起(首条 assistant 回复出现后)ID 必须与后续轮次一致,且与请求 ID 无关。
	turn3 := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello world"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"},{"role":"assistant","content":"done"},{"role":"user","content":"more"}]}`)
	third := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-3", turn3)
	if second.UpstreamID != third.UpstreamID || second.AffinityKey != third.AffinityKey {
		t.Fatalf("soft identity drifted across turns: second=%#v third=%#v", second, third)
	}
	// Different first user messages must remain isolated.
	other := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", []byte(`{"messages":[{"role":"user","content":"different"}]}`))
	if other.UpstreamID == first.UpstreamID {
		t.Fatal("different first user shared soft upstream id")
	}
	// 相同开头、不同助手回复的两个对话不得共享上游会话(历史串扰场景)。
	otherReply := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello world"},{"role":"assistant","content":"totally different answer"},{"role":"user","content":"next"}]}`)
	divergent := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-9", otherReply)
	if divergent.UpstreamID == second.UpstreamID {
		t.Fatal("conversations with different assistant replies shared soft upstream id")
	}
}

func TestResolveBuildSessionIdentitySoftFromResponsesInput(t *testing.T) {
	resolver := &IdentityResolver{}
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"cache me"}]}]}`)
	first := resolver.resolveTestIdentity(9, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", body)
	second := resolver.resolveTestIdentity(9, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", body)
	if first.UpstreamID == "" || first != second || !first.Soft {
		t.Fatalf("responses soft identity unstable: %#v %#v", first, second)
	}
	if first.ReplayKey != "" {
		t.Fatalf("responses soft identity must not enable reasoning replay: %#v", first)
	}
}

func TestResolveBuildSessionIdentityUsesInstructionsAsSystemAnchor(t *testing.T) {
	resolver := &IdentityResolver{}
	// Responses commonly uses top-level instructions instead of messages[system].
	a := resolver.resolveTestIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", []byte(`{"instructions":"stable system","input":[{"role":"user","content":"hello"}]}`))
	b := resolver.resolveTestIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-2", []byte(`{"instructions":"stable system","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`))
	b2 := resolver.resolveTestIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-3", []byte(`{"instructions":"stable system","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"},{"role":"assistant","content":"more"}]}`))
	c := resolver.resolveTestIdentity(3, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", []byte(`{"instructions":"other system","input":[{"role":"user","content":"hello"}]}`))
	if !a.Soft || a.UpstreamID == "" {
		t.Fatalf("expected soft identity: %#v", a)
	}
	if b.UpstreamID == "" || b.UpstreamID != b2.UpstreamID {
		t.Fatalf("instructions soft session unstable after first reply: %#v %#v", b, b2)
	}
	if a.UpstreamID == c.UpstreamID {
		t.Fatal("different instructions should isolate soft session")
	}
}

// 软会话按对话隔离而非按请求隔离: 相同内容的单轮请求复用同一上游会话
// (重复提问可命中前缀缓存), 账号亲和(AffinityKey)保持稳定; 请求 ID 不参与派生。
func TestSoftSessionUpstreamIDIsRequestScopedAffinityStable(t *testing.T) {
	resolver := &IdentityResolver{}
	body := []byte(`{"input":"same opening message"}`)
	first := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-1", body)
	second := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "request-2", body)
	if !first.Soft || !second.Soft {
		t.Fatalf("expected soft identities")
	}
	if first.UpstreamID != second.UpstreamID {
		t.Fatalf("identical single-turn requests must share the soft upstream id: %#v %#v", first, second)
	}
	if first.AffinityKey != second.AffinityKey {
		t.Fatalf("soft account affinity must stay stable")
	}
	if first.ReplayKey != "" || second.ReplayKey != "" {
		t.Fatalf("soft identities must not drive reasoning replay")
	}
	// 显式信号路径不受影响: scope 变化不改变身份(跨轮次/跨账号切换稳定)。
	a := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", "request-1", nil)
	b := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1", "request-2", nil)
	if a != b {
		t.Fatalf("explicit session identity must ignore request scope")
	}
}
