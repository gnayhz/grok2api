package history

import (
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"time"
)

func (r *IdentityResolver) resolveTestIdentity(id uint64, provider accountdomain.Provider, model, explicit, seed, scope string, body []byte) Identity {
	return r.resolveTestIdentityWithAnchors(id, provider, model, explicit, seed, scope, newBodyAnchors(body))
}
func (r *IdentityResolver) resolveTestIdentityWithAnchors(id uint64, provider accountdomain.Provider, model, explicit, seed, scope string, anchors *bodyAnchors) Identity {
	request := NewIdentityRequest(id, historydomain.ClientSignals{PromptCacheKey: seed}, explicit, scope, scope, nil)
	request.anchors = anchors
	return r.resolve(request, IdentityTarget{Provider: string(provider), Model: model})
}
func ensureTestComposerIdentity(identity Identity, id uint64, provider accountdomain.Provider, model, scope string) Identity {
	return ensureIsolatedIdentity(identity, id, IdentityTarget{Provider: string(provider), Model: model, IsolatedWithoutSession: provider == accountdomain.ProviderBuild && modeldomain.IsGrokComposerModel(model)}, scope)
}

func newSoftConversationRegistry() *softConversationRegistry {
	return &softConversationRegistry{entries: make(map[string]*softConversationEntry), lastSweep: time.Now()}
}
