package egress

// ProbeExecutionError means the local runtime could not complete a meaningful
// observation (admission, cancellation, shutdown or setup). It must not be
// persisted as an unhealthy exit or counted toward dead-exit confirmation.
// Unwrap preserves the original capacity/cancellation error for callers.
type ProbeExecutionError struct {
	Err error
}

func (e *ProbeExecutionError) Error() string { return "代理探测未完成: " + e.Err.Error() }
func (e *ProbeExecutionError) Unwrap() error { return e.Err }
