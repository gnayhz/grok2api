package provider

// Registry implementation, capability-projection half. The concrete
// registry lives at the dialect boundary ; port/provider keeps only
// the consumer interface and value types.

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	portprovider "github.com/chenyme/grok2api/backend/internal/port/provider"
)

func (r *Registry) SupportsStoredResponses(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.StoredResponses
}

func (r *Registry) SupportsStoredResponseModel(value account.Provider, model string) bool {
	if !r.SupportsStoredResponses(value) {
		return false
	}
	adapter, ok := r.Responses(value)
	if !ok {
		return false
	}
	if modelAdapter, ok := adapter.(portprovider.StoredResponseModelAdapter); ok {
		return modelAdapter.SupportsStoredResponseModel(model)
	}
	return true
}

func (r *Registry) SupportsConversation(value account.Provider, operation string) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.Supports(operation)
}

func (r *Registry) SupportsResponseCompaction(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.Compact
}

func (r *Registry) SupportsCredentialRefresh(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Credential.Refresh
}

func (r *Registry) QuotaKind(value account.Provider) (portprovider.QuotaKind, bool) {
	definition, ok := r.Definition(value)
	if !ok {
		return "", false
	}
	return definition.Quota, true
}

func (r *Registry) UsageKind(value account.Provider) (portprovider.UsageKind, bool) {
	definition, ok := r.Definition(value)
	if !ok {
		return "", false
	}
	return definition.Inference.Usage, true
}

func (r *Registry) RetryForbiddenAsEgress(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Inference.RetryForbiddenAsEgress
}

func (r *Registry) Responses(value account.Provider) (portprovider.ResponseAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.ResponseAdapter)
	return result, ok
}

func (r *Registry) Models(value account.Provider) (portprovider.ModelCatalogAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.ModelCatalogAdapter)
	return result, ok
}

func (r *Registry) Billing(value account.Provider) (portprovider.BillingAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.BillingAdapter)
	return result, ok
}

func (r *Registry) CredentialRefresh(value account.Provider) (portprovider.CredentialRefreshAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.CredentialRefreshAdapter)
	return result, ok
}

func (r *Registry) DeviceOAuth(value account.Provider) (portprovider.DeviceOAuthAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.DeviceOAuthAdapter)
	return result, ok
}

func (r *Registry) CredentialCodec(value account.Provider) (portprovider.CredentialCodecAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.CredentialCodecAdapter)
	return result, ok
}

// CredentialMetadata returns derived credential metadata safe for admin display.
func (r *Registry) CredentialMetadata(credential account.Credential) portprovider.CredentialMetadata {
	if r == nil {
		return portprovider.CredentialMetadata{}
	}
	adapter, ok := r.adapters[credential.Provider]
	if !ok {
		return portprovider.CredentialMetadata{}
	}
	inspector, ok := adapter.(portprovider.CredentialMetadataAdapter)
	if !ok {
		return portprovider.CredentialMetadata{}
	}
	return inspector.CredentialMetadata(credential)
}

func (r *Registry) AccountIdentity(value account.Provider) (portprovider.AccountIdentityAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.AccountIdentityAdapter)
	return result, ok
}

func (r *Registry) BuildConverter(value account.Provider) (portprovider.BuildCredentialConverter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.BuildCredentialConverter)
	return result, ok
}

func (r *Registry) Quota(value account.Provider) (portprovider.QuotaAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.QuotaAdapter)
	return result, ok
}

func (r *Registry) QuotaGroup(value account.Provider) (portprovider.QuotaGroupAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.QuotaGroupAdapter)
	return result, ok
}

// WebAccountSettings returns the Grok Web-specific account profile settings capability.
func (r *Registry) WebAccountSettings() (portprovider.WebAccountSettingsAdapter, bool) {
	adapter, ok := r.Get(account.ProviderWeb)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.WebAccountSettingsAdapter)
	return result, ok
}

func (r *Registry) QuotaMode(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return ""
	}
	metadata, ok := adapter.(portprovider.RoutingMetadataAdapter)
	if !ok {
		return ""
	}
	return metadata.QuotaMode(upstreamModel)
}

func (r *Registry) QuotaRefreshGroup(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return ""
	}
	metadata, ok := adapter.(portprovider.QuotaRefreshMetadataAdapter)
	if !ok {
		return ""
	}
	return metadata.QuotaRefreshGroup(upstreamModel)
}

func (r *Registry) TierOrder(value account.Provider, upstreamModel string) []account.WebTier {
	adapter, ok := r.Get(value)
	if !ok {
		return nil
	}
	metadata, ok := adapter.(portprovider.RoutingMetadataAdapter)
	if !ok {
		return nil
	}
	return metadata.TierOrder(upstreamModel)
}

func (r *Registry) TierOrderForQuotaMode(value account.Provider, upstreamModel, quotaMode string) []account.WebTier {
	adapter, ok := r.Get(value)
	if !ok {
		return nil
	}
	if metadata, ok := adapter.(portprovider.QuotaTierOrderAdapter); ok {
		return metadata.TierOrderForQuotaMode(upstreamModel, quotaMode)
	}
	metadata, ok := adapter.(portprovider.RoutingMetadataAdapter)
	if !ok {
		return nil
	}
	return metadata.TierOrder(upstreamModel)
}

func (r *Registry) PricingModel(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return upstreamModel
	}
	metadata, ok := adapter.(portprovider.PricingMetadataAdapter)
	if !ok {
		return upstreamModel
	}
	if model := metadata.PricingModel(upstreamModel); model != "" {
		return model
	}
	return upstreamModel
}

// ImageGeneration returns the image-generation capability registered by the Provider.
func (r *Registry) ImageGeneration(value account.Provider) (portprovider.ImageGenerationAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.ImageGenerationAdapter)
	return result, ok
}

// ImageEdit returns the image-editing capability registered by the Provider.
func (r *Registry) ImageEdit(value account.Provider) (portprovider.ImageEditAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.ImageEditAdapter)
	return result, ok
}

func (r *Registry) Videos(value account.Provider) (portprovider.VideoAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.VideoAdapter)
	return result, ok
}

func (r *Registry) TTS(value account.Provider) (portprovider.TTSAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.TTSAdapter)
	return result, ok
}

func (r *Registry) STT(value account.Provider) (portprovider.STTAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.STTAdapter)
	return result, ok
}

func (r *Registry) VoiceWebSocket(value account.Provider) (portprovider.VoiceWebSocketAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(portprovider.VoiceWebSocketAdapter)
	return result, ok
}
