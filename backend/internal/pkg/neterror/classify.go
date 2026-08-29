package neterror

import (
	"errors"
	"net"
	"strings"
)

const responseHeaderTimeoutMarker = "timeout awaiting response headers"

// ErrUpstreamStreamIdleTimeout is attached to a request context when a
// provider streaming response is aborted because no data arrived within the
// configured idle window.
var ErrUpstreamStreamIdleTimeout = errors.New("upstream stream idle timeout")

// ErrUpstreamOutputLoop is raised when the gateway terminates a stream because
// the model repeated the same visible or reasoning delta past the doom-loop
// guard. It is distinct from a transport cut (upstream_stream_interrupted).
var ErrUpstreamOutputLoop = errors.New("model output loop detected")

// IsResponseHeaderTimeout identifies the HTTP/1.1 and HTTP/2 timeout values
// returned by the Go transport while waiting for the first response headers.
func IsResponseHeaderTimeout(err error) bool {
	if err == nil {
		return false
	}
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), responseHeaderTimeoutMarker)
}

// IsUpstreamStreamIdleTimeout reports whether err is (or wraps) the shared
// provider stream-idle timeout sentinel.
func IsUpstreamStreamIdleTimeout(err error) bool {
	return errors.Is(err, ErrUpstreamStreamIdleTimeout)
}

// IsUpstreamOutputLoop reports whether the stream was aborted by the repeated
// delta doom-loop guard rather than a transport interrupt.
func IsUpstreamOutputLoop(err error) bool {
	return errors.Is(err, ErrUpstreamOutputLoop)
}
