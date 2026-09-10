package inference

// RequestValidationError describes a locally rejected client request. Message
// must be safe to return to that client; upstream error bodies cannot create it.
type RequestValidationError struct {
	Code    string
	Param   string
	Message string
}

func (e *RequestValidationError) Error() string { return e.Message }
