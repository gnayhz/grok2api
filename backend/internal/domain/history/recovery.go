package history

// Rejection is a fact established by the upstream protocol adapter. It is not
// permission to alter a conversation or perform another generation.
type Rejection string

const OpaqueDecodeRejected Rejection = "opaque_decode_rejected"

type RecoveryMode uint8

const (
	PreserveOpaque RecoveryMode = iota
	AllowLossyRecovery
)

type RecoveryAction string

const (
	RemoveRejectedOpaque     RecoveryAction = "remove_rejected_opaque"
	ClearUpstreamSessionHint RecoveryAction = "clear_upstream_session_hint"
)

// RecoveryStep is an explicit, potentially lossy proposal from the history owner.
// Body retains all portable input; clearing the upstream hint never changes scope.
type RecoveryStep struct {
	Action            RecoveryAction
	Body              []byte
	RemovedOpaque     int
	InvalidateHistory bool
	ClearSessionHint  bool
}

type RecoveryInput struct {
	Rejection      Rejection
	Method         string
	Body           []byte
	PromptCacheKey string
}

type RecoveryOutcome struct {
	OmittedCompactions         int
	IdentityContextUnavailable bool
	Actions                    []RecoveryAction
	RemovedOpaque              int
	SessionHintCleared         bool
	Failed                     bool
	Reason                     string
}
