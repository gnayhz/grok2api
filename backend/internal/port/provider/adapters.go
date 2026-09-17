package provider

import (
	"context"
	"io"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

// Adapter defines only Provider identity; concrete capabilities are registered through small interfaces as needed.
type Adapter interface {
	Provider() account.Provider
}

type ResponseAdapter interface {
	Adapter
	ForwardResponse(ctx context.Context, request ResponseResourceRequest) (*Response, error)
}

type ModelCatalogAdapter interface {
	Adapter
	ListModels(ctx context.Context, credential account.Credential) ([]string, error)
}

// AccountModelCapabilityNormalizer is optional and normalizes account model capabilities from Billing and credential entitlement.
// Without it, model sync writes the upstream catalog unchanged; nil billing means Unknown with no snapshot.
// credential is used for Build Super entitlement; default Providers may ignore it.
type AccountModelCapabilityNormalizer interface {
	Adapter
	NormalizeAccountModelCapabilities(models []string, billing *account.Billing, credential account.Credential) []string
}

type BillingAdapter interface {
	Adapter
	GetBilling(ctx context.Context, credential account.Credential) (account.Billing, error)
}

type CredentialRefreshAdapter interface {
	Adapter
	RefreshCredential(ctx context.Context, credential account.Credential) (RefreshedCredential, error)
}

type DeviceOAuthAdapter interface {
	Adapter
	StartDeviceAuthorization(ctx context.Context) (DeviceAuthorization, error)
	PollDeviceAuthorization(ctx context.Context, deviceCode string) (CredentialSeed, error)
}

type CredentialCodecAdapter interface {
	Adapter
	ParseImportedCredentials(data []byte) ([]CredentialSeed, error)
	MarshalCredentials(values []CredentialSeed) ([]byte, error)
}

// CredentialImportPreparer exchanges incomplete imported credentials before
// persistence. Implementations must preserve provider token rotation.
type CredentialImportPreparer interface {
	Adapter
	PrepareImportedCredential(ctx context.Context, seed CredentialSeed) (CredentialSeed, error)
}

type CredentialMetadataAdapter interface {
	Adapter
	CredentialMetadata(credential account.Credential) CredentialMetadata
}

type AccountIdentityAdapter interface {
	Adapter
	SyncAccountIdentity(ctx context.Context, credential account.Credential) (AccountIdentity, error)
}

type BuildCredentialConverter interface {
	Adapter
	ConvertToBuild(ctx context.Context, credential account.Credential) (CredentialSeed, error)
}

type QuotaAdapter interface {
	Adapter
	SyncQuota(ctx context.Context, credential account.Credential) (QuotaSnapshot, error)
	SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error)
}

// QuotaGroupAdapter is optional. It is used when one upstream endpoint returns
// several related quota products as one authoritative response.
type QuotaGroupAdapter interface {
	Adapter
	SyncQuotaGroup(ctx context.Context, credential account.Credential, group string) (QuotaGroupSnapshot, error)
}

// QuotaRefreshMetadataAdapter maps a model to an internal refresh group. The
// group is scheduling metadata, not a routable quota mode.
type QuotaRefreshMetadataAdapter interface {
	Adapter
	QuotaRefreshGroup(upstreamModel string) string
}

// WebAccountSettingsAdapter defines upstream profile-setting capabilities for Grok Web SSO accounts.
// This capability belongs only to the Web Provider; Build and Console must not emulate it through generic account logic.
type WebAccountSettingsAdapter interface {
	Adapter
	AcceptTerms(ctx context.Context, credential account.Credential) error
	SetBirthDate(ctx context.Context, credential account.Credential, birthDate time.Time) error
	EnableNSFW(ctx context.Context, credential account.Credential) error
}

// ImageGenerationAdapter defines an optional Provider image-generation capability.
type ImageGenerationAdapter interface {
	Adapter
	GenerateImage(ctx context.Context, request ImageGenerationRequest) (*Response, error)
}

// ImageEditAdapter defines an optional Provider image-editing capability.
type ImageEditAdapter interface {
	Adapter
	EditImage(ctx context.Context, request ImageEditRequest) (*Response, error)
}

// ImageAssetStore archives generated images as local resources that the backend can read reliably.
type ImageAssetStore interface {
	SaveImage(ctx context.Context, data []byte) (media.Asset, error)
	PublicImageURL(id string) string
}

type VideoAdapter interface {
	Adapter
	GenerateVideo(ctx context.Context, request VideoRequest) (VideoResult, error)
}

// VideoContentDownloader reads completed video content using the credential that created the task.
// Callers must verify task ownership first.
type VideoContentDownloader interface {
	VideoAdapter
	DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error)
}

// TTSAdapter synthesizes speech audio for text prompts.
type TTSAdapter interface {
	Adapter
	SynthesizeSpeech(ctx context.Context, request TTSRequest) (TTSResult, error)
	ListTTSVoices(ctx context.Context, credential account.Credential) ([]VoiceInfo, error)
	GetTTSVoice(ctx context.Context, credential account.Credential, voiceID string) (VoiceInfo, error)
}

// STTAdapter transcribes audio into text.
type STTAdapter interface {
	Adapter
	TranscribeSpeech(ctx context.Context, request STTRequest) (STTResult, error)
}

// VoiceWebSocketConn is a minimal duplex websocket used by voice streaming proxies.
type VoiceWebSocketConn interface {
	ReadMessage() (messageType int, data []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadLimit(limit int64)
	Close() error
}

// VoiceWebSocketAdapter dials official voice websocket endpoints with account auth.
type VoiceWebSocketAdapter interface {
	Adapter
	// Prepare validates/copies protocol options without credentials, I/O or side effects.
	PrepareVoiceWebSocket(VoiceWebSocketRequest) (VoiceWebSocketRequest, error)
	DialVoiceWebSocket(ctx context.Context, request VoiceWebSocketRequest) (VoiceWebSocketConn, func(), error)
}

type RoutingMetadataAdapter interface {
	Adapter
	QuotaMode(upstreamModel string) string
	TierOrder(upstreamModel string) []account.WebTier
}

// QuotaTierOrderAdapter optionally narrows account tiers for a concrete quota
// product. It is used when one public model exposes parameter variants backed
// by different upstream entitlements.
type QuotaTierOrderAdapter interface {
	TierOrderForQuotaMode(upstreamModel, quotaMode string) []account.WebTier
}

// PricingMetadataAdapter maps Provider-private model identifiers to public billing models.
type PricingMetadataAdapter interface {
	Adapter
	PricingModel(upstreamModel string) string
}

// StoredResponseModelAdapter narrows a provider-wide storage capability to the
// model's actual protocol. Temporary compatibility response IDs carry no history.
type StoredResponseModelAdapter interface {
	SupportsStoredResponseModel(model string) bool
}
