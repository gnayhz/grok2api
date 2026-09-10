package neterror

import "errors"

// LocalExecutionError preserves a local execution owner's stop reason through
// transport/provider wrappers without treating it as evidence against a peer.
type LocalExecutionError struct {
	Err              error
	BeforeSubmission bool
}

func (e *LocalExecutionError) Error() string { return "local execution stopped: " + e.Err.Error() }
func (e *LocalExecutionError) Unwrap() error { return e.Err }
func IsLocalExecution(err error) bool {
	var local *LocalExecutionError
	return errors.As(err, &local)
}

func RejectedBeforeSubmission(err error) bool {
	var local *LocalExecutionError
	return errors.As(err, &local) && local.BeforeSubmission
}
