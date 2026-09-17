package provider

// Registry is the consumer contract for the provider registry: capability
// queries, definition reads, dialect adapter projections and assembly
// validation. Registration/resolution/capability computation lives in
// infra/provider ; the composition root injects the concrete
// registry through this interface.

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

type Registry interface {
	SupportsStoredResponses(value account.Provider) bool
	SupportsStoredResponseModel(value account.Provider, model string) bool
	SupportsConversation(value account.Provider, operation string) bool
	SupportsResponseCompaction(value account.Provider) bool
	SupportsCredentialRefresh(value account.Provider) bool
	QuotaKind(value account.Provider) (QuotaKind, bool)
	UsageKind(value account.Provider) (UsageKind, bool)
	RetryForbiddenAsEgress(value account.Provider) bool
	Responses(value account.Provider) (ResponseAdapter, bool)
	Models(value account.Provider) (ModelCatalogAdapter, bool)
	Billing(value account.Provider) (BillingAdapter, bool)
	CredentialRefresh(value account.Provider) (CredentialRefreshAdapter, bool)
	DeviceOAuth(value account.Provider) (DeviceOAuthAdapter, bool)
	CredentialCodec(value account.Provider) (CredentialCodecAdapter, bool)
	CredentialMetadata(credential account.Credential) CredentialMetadata
	AccountIdentity(value account.Provider) (AccountIdentityAdapter, bool)
	BuildConverter(value account.Provider) (BuildCredentialConverter, bool)
	Quota(value account.Provider) (QuotaAdapter, bool)
	QuotaGroup(value account.Provider) (QuotaGroupAdapter, bool)
	WebAccountSettings() (WebAccountSettingsAdapter, bool)
	QuotaMode(value account.Provider, upstreamModel string) string
	QuotaRefreshGroup(value account.Provider, upstreamModel string) string
	TierOrder(value account.Provider, upstreamModel string) []account.WebTier
	TierOrderForQuotaMode(value account.Provider, upstreamModel, quotaMode string) []account.WebTier
	PricingModel(value account.Provider, upstreamModel string) string
	ImageGeneration(value account.Provider) (ImageGenerationAdapter, bool)
	ImageEdit(value account.Provider) (ImageEditAdapter, bool)
	Videos(value account.Provider) (VideoAdapter, bool)
	TTS(value account.Provider) (TTSAdapter, bool)
	STT(value account.Provider) (STTAdapter, bool)
	VoiceWebSocket(value account.Provider) (VoiceWebSocketAdapter, bool)
	Get(value account.Provider) (Adapter, bool)
	Definition(value account.Provider) (Definition, bool)
	Providers() []account.Provider
	Validate() error
}
