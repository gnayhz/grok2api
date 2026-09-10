package cli

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

// egressTransport consumes M13's routing and lease contract. Quality decorators
// may observe selected paths without owning route selection. Built-in direct
// traffic also acquires a managed lease; custom fallback transports are retained.
type egressTransport struct {
	manager  infraegress.Dialer
	fallback http.RoundTripper
}

func (t *egressTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	affinity := infraegress.AccountFromContext(request.Context())
	if affinity == "" {
		affinity = "bootstrap"
	}
	// Routing (traffic class -> scope -> default -> automatic) resolves inside the
	// manager, so the transport only decides between a routed lease and the
	// process-wide fallback transport.
	lease, configured, err := t.manager.AcquireIfConfigured(request.Context(), domainegress.ScopeBuild, affinity)
	if err != nil {
		return nil, err
	}
	if !configured {
		// The production fallback is managed even without account isolation.
		// Explicitly injected custom transports retain their existing behavior.
		if _, builtIn := t.fallback.(*buildDirectTransport); builtIn {
			lease, err = t.manager.AcquireBuildEnvironmentDirect(request.Context(), affinity)
			if err != nil {
				return nil, err
			}
			return t.roundTripWithLease(request, lease)
		}
		// When account-isolated pools are enabled, still go through the manager's
		// direct node so different accounts do not share the process-wide fallback
		// HTTP transport / TCP connection pool. Preserve the fallback transport's
		// HTTP_PROXY/HTTPS_PROXY behavior while partitioning the pool.
		lease, configured, err = t.manager.AcquireBuildEnvironmentDirectIfIsolated(request.Context(), affinity)
		if err != nil {
			return nil, err
		}
		if !configured {
			idleRequest := t.withStreamIdleContext(request)
			idleRequest = idleRequest.WithContext(attemptmeta.Begin(idleRequest.Context(), attemptmeta.Path{Status: attemptmeta.PathUnknown}))
			if err := infraegress.BeginDirectPhysicalCall(idleRequest.Context()); err != nil {
				if idleRequest.Body != nil {
					_ = idleRequest.Body.Close()
				}
				return nil, err
			}
			response, requestErr := t.fallback.RoundTrip(idleRequest)
			requestErr = infraegress.MarkPhysicalExecutionError(idleRequest.Context(), requestErr)
			attemptmeta.Attach(response, idleRequest)
			infraegress.RecordDirectPhysicalCall(idleRequest.Context(), response, requestErr)
			if requestErr != nil && response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if requestErr != nil || response == nil || response.Body == nil {
				return response, requestErr
			}
			response.Body = t.wrapStreamIdleBody(response.Body, idleRequest.Context())
			return response, requestErr
		}
	}
	return t.roundTripWithLease(request, lease)
}

// roundTripWithLease executes one upstream request through an acquired egress
// lease with the shared User-Agent, feedback, stream-idle, and release
// behavior used by every Build transport path.
func (t *egressTransport) roundTripWithLease(request *http.Request, lease *infraegress.Lease) (*http.Response, error) {
	if lease.UserAgent != "" {
		request.Header.Set("User-Agent", lease.UserAgent)
	}
	// A panic inside lease.Do or the wrapping helpers must not leak the
	// inflight slot: Release is idempotent (sync.Once), so a deferred guard
	// composes safely with the explicit releases below and the body wrapper.
	released := false
	defer func() {
		if !released {
			lease.Release()
		}
	}()
	idleRequest := t.withStreamIdleContext(request)
	response, err := lease.Do(idleRequest)
	if err != nil {
		if shouldReportEgressFailure(request.Context(), err) {
			lease.Observe(0, err)
		}
		released = true
		lease.Release()
		return nil, err
	}
	lease.Observe(response.StatusCode, nil)
	if response.Body == nil {
		released = true
		lease.Release()
		return response, nil
	}
	response.Body = &egressResponseBody{ReadCloser: t.wrapStreamIdleBody(response.Body, idleRequest.Context()), release: lease.Release}
	// Ownership transfers to the body wrapper; the deferred guard must not
	// release again after a successful handoff.
	released = true
	return response, nil
}

// withStreamIdleContext returns a shallow copy of request carrying a
// cancel-cause-aware context derived from the original. The cancel function is
// stashed on the request context so wrapStreamIdleBody can arm an idle timer
// that cancels the context (and thus the transport's body read) when the
// stream goes silent. When no idle timeout is configured the original request
// is returned unchanged.
func (t *egressTransport) withStreamIdleContext(request *http.Request) *http.Request {
	if !acceptsEventStream(request.Header.Values("Accept")) {
		return request
	}
	idle := t.manager.BuildStreamIdleTimeout()
	if idle <= 0 {
		return request
	}
	ctx, cancel := context.WithCancelCause(request.Context())
	return request.Clone(withIdleCancel(ctx, idle, cancel))
}

// acceptsEventStream keeps stream-idle enforcement scoped to requests that
// explicitly negotiate SSE. The Build HTTP client is shared by inference,
// OAuth, models, billing, and media calls, so applying the timeout solely from
// the egress scope would also abort legitimate non-streaming response bodies.
func acceptsEventStream(values []string) bool {
	for _, value := range values {
		for _, candidate := range strings.Split(value, ",") {
			mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(candidate))
			if err == nil && strings.EqualFold(mediaType, "text/event-stream") {
				return true
			}
		}
	}
	return false
}

// wrapStreamIdleBody arms an idle timer over body. The cancel function is read
// from the request context previously installed by withStreamIdleContext. When
// no cancel is present (idle disabled) the body is returned unwrapped.
func (t *egressTransport) wrapStreamIdleBody(body io.ReadCloser, ctx context.Context) io.ReadCloser {
	idle, cancel := idleCancelFrom(ctx)
	if idle <= 0 || cancel == nil {
		return body
	}
	return newIdleTimeoutReadCloser(body, idle, cancel)
}

func shouldReportEgressFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, context.Canceled)
}

type egressResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *egressResponseBody) finish() {
	b.once.Do(func() {
		if b.release != nil {
			b.release()
		}
	})
}

func (b *egressResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *egressResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}
