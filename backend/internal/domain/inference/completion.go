package inference

import "errors"

var (
	// ErrCompletionCommit means a required success barrier was not acknowledged.
	ErrCompletionCommit        = errors.New("completion_commit_failed")
	ErrProviderStateCommit     = errors.New("provider_state_commit_failed")
	ErrResponseOwnershipCommit = errors.New("response_ownership_commit_failed")
)
