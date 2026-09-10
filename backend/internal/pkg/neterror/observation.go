package neterror

import "errors"

// TransportPhase identifies errors obtained directly from the upstream
// transport. Provider decoding, downstream writes and business errors never
// acquire this marker merely because they are passed to a completion callback.
type TransportPhase string

const (
	PhaseRequest      TransportPhase = "request"
	PhaseResponseBody TransportPhase = "response_body"
	PhaseWebSocket    TransportPhase = "websocket_handshake"
)

type TransportError struct {
	Phase TransportPhase
	Err   error
}

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }
func MarkTransport(err error, phase TransportPhase) error {
	if err == nil {
		return nil
	}
	var observed *TransportError
	if errors.As(err, &observed) {
		return err
	}
	return &TransportError{Phase: phase, Err: err}
}
func IsTransport(err error) bool { var observed *TransportError; return errors.As(err, &observed) }
