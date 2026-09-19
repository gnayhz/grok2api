package model

// ProbeFailure preserves source/stage/cause in the persisted failure_kind.
// These codes are emitted at the failing operation, never inferred from an
// error message. Legacy codes have no trustworthy provenance and abstain.
type ProbeFailure string

const (
	ProbeFailureUnknown          ProbeFailure = "unknown/measurement/error"
	ProbeFailureConfiguration    ProbeFailure = "local/prepare/configuration"
	ProbeFailureAccount          ProbeFailure = "local/prepare/account"
	ProbeFailureCredential       ProbeFailure = "local/prepare/credential"
	ProbeFailureCapacity         ProbeFailure = "local/prepare/capacity"
	ProbeFailureIdentity         ProbeFailure = "local/verification/identity"
	ProbeFailurePath             ProbeFailure = "local/verification/path"
	ProbeFailurePathRegistration ProbeFailure = "local/verification/path_unregistered"
	ProbeFailurePolicy           ProbeFailure = "local/prepare/policy"
	ProbeFailureExperiment       ProbeFailure = "local/verification/experiment"
	ProbeFailureInterrupted      ProbeFailure = "local/measurement/interrupted"
	ProbeFailureResource         ProbeFailure = "local/measurement/resource_limit"
	ProbeFailurePersistence      ProbeFailure = "local/record/persistence"
	ProbeFailureForward          ProbeFailure = "unknown/forward/error"
	ProbeFailureResponse         ProbeFailure = "unknown/forward/empty_response"
	ProbeFailureHTTPRejected     ProbeFailure = "upstream/headers/request_rejected"
	ProbeFailureHTTPServer       ProbeFailure = "upstream/headers/server_error"
	ProbeFailureCreatedTimeout   ProbeFailure = "upstream/admission/created_timeout"
	ProbeFailureEvidenceTimeout  ProbeFailure = "upstream/admission/evidence_timeout"
	ProbeFailureEmptyStream      ProbeFailure = "upstream/admission/empty_stream"
	ProbeFailureTruncatedStream  ProbeFailure = "upstream/admission/truncated_stream"
	ProbeFailureProtocol         ProbeFailure = "local/admission/unsupported_protocol"
	ProbeFailureAdmission        ProbeFailure = "unknown/admission/error"
	ProbeFailureCompletion       ProbeFailure = "unknown/completion/error"
	ProbeFailureCompletionBudget ProbeFailure = "local/completion/deadline"
)

// SupportsAvailability permits only failures observed on the upstream response
// boundary. It does not establish attribution: identity, independent paths and
// matched controls still have to satisfy the court's experiment protocol.
func (f ProbeFailure) SupportsAvailability() bool {
	switch f {
	case ProbeFailureHTTPServer, ProbeFailureCreatedTimeout, ProbeFailureEvidenceTimeout,
		ProbeFailureEmptyStream, ProbeFailureTruncatedStream:
		return true
	}
	return false
}
