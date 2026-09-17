package provider

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// ResponseResourceRequest describes a common upstream request to a Responses resource endpoint.
type ResponseResourceRequest struct {
	// ObserveImage reports actual generation for image models exposed through text protocols.
	// The callback is cumulative within this invocation and precedes post-processing.
	ObserveImage func(ImageGenerationObservation)
	Credential   account.Credential
	// Billing is used only to determine XAI eligibility in Build auto mode; nil means the account tier is unknown.
	Billing        *account.Billing
	Method         string
	Path           string
	Body           []byte
	Model          string
	PromptCacheKey string
	// ReasoningReplayKey comes only from explicit client session identity; soft cache identity must not replay ciphertext.
	ReasoningReplayKey string
	// PriorReasoningReplayKey is an ambiguous pre-encoding scope, for loss checks only.
	PriorReasoningReplayKey string
	// ToolCompatibilityPolicy is supplied by the logical request owner.
	ToolCompatibilityPolicy inferencedomain.ToolCompatibilityPolicy
	// GrokTurnIndex is the explicit Grok Shell client turn; it is validated before Build egress and never fabricated by the server.
	GrokTurnIndex string
	IdempotencyID string
	Streaming     bool
	NormalizeBody bool
	Operation     string
	// DeferOutputCommit keeps response-derived cache writes pending until the
	// gateway confirms complete delivery. Rejected or interrupted attempts must
	// have no such effects.
	DeferOutputCommit bool
	// DisableAutomaticReplay forbids internal fallbacks after an ambiguous attempt.
	DisableAutomaticReplay bool
	// HistoryControl owns permission for input preparation and rejection recovery.
	HistoryControl HistoryController
	// NormalizedMetadata receives non-sensitive metadata from the exact payload
	// normalization used for the physical upstream request. The caller owns the
	// value; adapters update it synchronously before network I/O.
	NormalizedMetadata *NormalizedRequestMetadata
	// OnNormalized runs synchronously before network I/O with the actual tool
	// profile. It must be fast and may abort the request (e.g. exhausted budget).
	OnNormalized func(NormalizedRequestMetadata) error
}

// NormalizedRequestMetadata contains safe request attributes that may be kept
// in audit records. It must never contain request content or credentials.
type NormalizedRequestMetadata struct {
	// ImageOutputCount is the validated request count for reserving a budget.
	// It is never evidence that any image was generated.
	ImageOutputCount  int
	ToolCompatibility *inferencedomain.ToolCompatibilityPlan
	ReplayPolicy      *inferencedomain.ReplayPolicy
	ReasoningEffort   string
}

// Response represents an upstream response that has not yet been written downstream.
type Response struct {
	// RequestValidation is set only by local parsing/compatibility checks before
	// contacting upstream. It must never be inferred from an upstream HTTP body.
	RequestValidation *inferencedomain.RequestValidationError
	Attempt           attemptmeta.Identity
	StatusCode        int
	Status            string
	Header            http.Header
	Body              io.ReadCloser
	QuotaUnits        int
	UpstreamURL       string
	Diagnostic        *DiagnosticResponse
	// PolicyForbidden preserves a Provider-classified origin policy rejection.
	// A generic egress retry cannot repair this rejection; it is distinct from
	// local request validation and browser/clearance failures.
	PolicyForbidden bool
	// RecoveredPrimaryFailure records a primary-plane failure hidden by a successful Provider fallback.
	RecoveredPrimaryFailure *DiagnosticResponse
	RateLimit               *RateLimitMetadata
	// ModelCatalogChanged indicates that the model catalog ETag in an inference response differs from
	// the ETag from the account's most recent successful /models sync.
	ModelCatalogChanged bool
	// ConvertStream, if set, turns the raw upstream SSE into the client
	// protocol (chat/messages). The gateway must quality-peek the raw body
	// first: converting before peek delays thinking evidence until item.done
	// and inflates client TTFB to the full reasoning duration.
	ConvertStream func(io.ReadCloser) io.ReadCloser
	// ConvertJSON is the non-stream analog: peek the raw Responses JSON,
	// then convert. Converting first drops thinking into optional chat
	// fields and makes usage.reasoning_tokens look like a finished answer.
	ConvertJSON func([]byte) ([]byte, error)
	// AcceptOutput permits deferred response-derived cache writes. It is safe
	// to call after complete successful delivery and its required completion
	// receipt, even if the original body has already closed. Nil has no effect.
	AcceptOutput func()
	// CommitOutput persists accepted history before client success termination.
	CommitOutput func() error
	// CommitResponseState acknowledges native continuation/resource state (Web)
	// before success. Gateway owns the deadline and public completion boundary.
	CommitResponseState  func(context.Context) error
	HistoryOutcome       string
	HistoryScopeHash     string
	HistoryGeneration    int64
	HistoryRestoredItems int
	HistoryNormalizer    int
	// DiscardOutput releases pending cache retention for a rejected attempt or
	// interrupted delivery. Call it even when Body has already been closed.
	DiscardOutput func()
}

const (
	RateLimitScopeRPS = "rps"
	RateLimitScopeRPM = "rpm"
)

// RateLimitMetadata contains transient rate-limit metadata that is safe to propagate from upstream.
type RateLimitMetadata struct {
	Scope      string
	TeamID     string
	Model      string
	Actual     int
	Limit      int
	RetryAfter time.Duration
}

const MaxDiagnosticBodyBytes = 64 << 10

// DiagnosticResponse retains a size-limited failure response before Provider conversion.
type DiagnosticResponse struct {
	StatusCode    int
	Status        string
	Header        http.Header
	Body          []byte
	BodyTruncated bool
}

// ReadDiagnosticBody reads up to the diagnostic body limit and reports whether upstream content was truncated.
func ReadDiagnosticBody(body io.Reader) ([]byte, bool, error) {
	if body == nil {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, MaxDiagnosticBodyBytes+1))
	if len(data) <= MaxDiagnosticBodyBytes {
		return data, false, err
	}
	return data[:MaxDiagnosticBodyBytes], true, err
}

// DeviceAuthorization represents the result of starting Device OAuth.
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresIn               time.Duration
}

// CredentialSeed represents an OAuth credential not yet persisted after login or import.
type CredentialSeed struct {
	Provider                account.Provider
	AuthType                account.AuthType
	WebTier                 account.WebTier
	Name                    string
	Email                   string
	UserID                  string
	TeamID                  string
	SourceKey               string
	OIDCClientID            string
	AccessToken             string
	RefreshToken            string
	CloudflareCookies       string
	ExpiresAt               time.Time
	WebNSFWEnabledAt        *time.Time
	WebTermsAcceptedAt      *time.Time
	WebTermsAcceptedVersion int
	WebBirthDateSetAt       *time.Time
}

type QuotaSnapshot struct {
	Tier     account.WebTier
	Windows  []account.QuotaWindow
	SyncedAt time.Time
}

// QuotaGroupSnapshot is an authoritative snapshot for a group of quota modes
// returned by one upstream request. Modes lists the complete local scope so
// callers can atomically remove products that the upstream explicitly reports
// as unavailable without touching unrelated quota windows.
type QuotaGroupSnapshot struct {
	Group    string
	Modes    []string
	Windows  []account.QuotaWindow
	SyncedAt time.Time
}

type ImageGenerationRequest struct {
	Observe        func(ImageGenerationObservation)
	Credential     account.Credential
	Model          string
	Prompt         string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
}

type ImageInput struct {
	Filename string
	MIMEType string
	Data     []byte
}

type ImageEditRequest struct {
	Observe        func(ImageGenerationObservation)
	Credential     account.Credential
	Model          string
	Prompt         string
	ImageURLs      []string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
}

// ImageGenerationObservation contains cumulative protocol facts for one
// Adapter invocation. Counts describe confirmed final images before download
// or storage; previews and requested n are not generated output. The callback
// may run in a body producer, which Body.Close must join before returning.
type ImageGenerationObservation struct {
	Started, Completed, Failed bool
	OutputImages, QuotaUnits   int
	UpstreamStatus             int
}

func ObserveImageGeneration(observe func(ImageGenerationObservation), fact ImageGenerationObservation) {
	if observe != nil {
		observe(fact)
	}
}

type TTSOutputFormat struct {
	Codec      string
	SampleRate int
	BitRate    int
}

type TTSRequest struct {
	Credential               account.Credential
	Model                    string
	Text                     string
	VoiceID                  string
	Language                 string
	OutputFormat             TTSOutputFormat
	Speed                    float64
	OptimizeStreamingLatency int
	TextNormalization        bool
	WithTimestamps           bool
}

type TTSTimestampSpan struct {
	Start float64
	End   float64
}

type TTSTimestamps struct {
	GraphChars []string
	GraphTimes []TTSTimestampSpan
}

type TTSResult struct {
	// InputCharacters is the exact Unicode count sent upstream.
	InputCharacters int
	Audio           []byte
	ContentType     string
	Duration        float64
	Base64Audio     string
	Timestamps      *TTSTimestamps
	JSONEnvelope    bool
}

type STTRequest struct {
	Credential   account.Credential
	Model        string
	FileName     string
	FileMIME     string
	FileData     []byte
	URL          string
	AudioFormat  string
	SampleRate   string
	Language     string
	Format       bool
	Multichannel bool
	Channels     int
	Diarize      bool
	KeyTerms     []string
	FillerWords  bool
	VADThreshold *float64
}

type STTWord struct {
	Text    string
	Start   float64
	End     float64
	Speaker *int
}

type STTChannel struct {
	Index int
	Text  string
	Words []STTWord
}

type STTResult struct {
	DurationReported bool
	Text             string
	Language         string
	Duration         float64
	Words            []STTWord
	Channels         []STTChannel
	RawJSON          []byte
}

type VoiceInfo struct {
	VoiceID  string
	Name     string
	Language string
}

// RefreshedCredential represents rotated credentials returned by an OAuth refresh.
type RefreshedCredential struct {
	EncryptedAccessToken  string
	EncryptedRefreshToken string
	ExpiresAt             time.Time
	// RefreshTokenRotated reports that the OAuth response explicitly returned
	// a different refresh token. It is diagnostic metadata only; token values
	// must never be logged.
	RefreshTokenRotated bool
}

// CredentialMetadata contains non-sensitive display data safely derived from a stored credential.
// Raw tokens and complete JWT claims must never be exposed through this structure.
type CredentialMetadata struct {
	// BuildBotFlagInspected is true only when the Build token was successfully
	// decrypted and decoded. False means the risk source is unknown, not clean.
	BuildBotFlagInspected bool
	// BuildBotFlagged is true when BuildBotFlagSource is 1 or 2.
	BuildBotFlagged bool
	// BuildBotFlagSource is the numeric bot_flag_source/bfs claim (1 or 2), or 0 when unset.
	BuildBotFlagSource int
}

// AccountIdentity contains non-sensitive account identity metadata confirmed by upstream.
// Email is for display only; cross-Provider automatic linking uses stable UserID only.
type AccountIdentity struct {
	Email  string
	UserID string
	TeamID string
}

// VoiceWebSocketRequest dials an upstream voice websocket with provider auth.
type VoiceWebSocketRequest struct {
	Credential account.Credential
	// Path is a v1-relative path such as /realtime or /stt.
	Path  string
	Model string
	Query url.Values
	// Observe publishes protocol facts before the transport forwards the frame.
	// Persistence, delivery and accounting decisions remain with the caller.
	Observe func(VoiceWebSocketObservation)
}

type VoiceWebSocketObservation struct {
	Started, Completed, Partial, Failed bool
	AudioDurationSeconds                float64
	AudioDurationReported               bool
}
