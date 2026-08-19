package cli

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

type egressTransport struct {
	manager  *infraegress.Manager
	fallback http.RoundTripper
}

func (t *egressTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	affinity := infraegress.AccountFromContext(request.Context())
	if affinity == "" {
		affinity = "bootstrap"
	}
	if lease, routed := t.acquireRouteRuleLease(request.Context(), domainegress.ScopeBuild, affinity); routed {
		return t.roundTripWithLease(request, lease)
	}
	lease, configured, err := t.manager.AcquireIfConfigured(request.Context(), domainegress.ScopeBuild, affinity)
	if err != nil {
		return nil, err
	}
	if !configured {
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
			response, requestErr := t.fallback.RoundTrip(idleRequest)
			infraegress.RecordDirectPhysicalCall(request.Context(), response, requestErr)
			if requestErr != nil || response == nil || response.Body == nil {
				return response, requestErr
			}
			response.Body = t.wrapStreamIdleBody(response.Body, idleRequest.Context())
			return response, requestErr
		}
	}
	return t.roundTripWithLease(request, lease)
}

// acquireRouteRuleLease resolves a traffic-class route rule for one Build
// upstream call. The bool result reports that the rule supplied a usable
// lease; when the configured target is unavailable the call falls back to the
// ordinary scope-pool selection instead of failing.
func (t *egressTransport) acquireRouteRuleLease(ctx context.Context, scope domainegress.Scope, affinity string) (*infraegress.Lease, bool) {
	class := infraegress.TrafficClassFromContext(ctx)
	decision := t.manager.RouteRuleFor(ctx, scope, class)
	if !decision.Applied {
		return nil, false
	}
	switch decision.Rule.TargetMode.Normalized() {
	case domainegress.RouteRuleTargetDirect:
		lease, err := t.manager.AcquireRoutedDirect(ctx, scope, affinity)
		if err != nil {
			infraegress.RecordRouteRuleOutcome(scope, class, infraegress.RouteRuleOutcomeDirectUnavailable)
			return nil, false
		}
		infraegress.RecordRouteRuleOutcome(scope, class, infraegress.RouteRuleOutcomeHit)
		return lease, true
	default:
		lease, err := t.manager.AcquireRouted(ctx, scope, affinity, decision.Rule.TargetNodeID)
		if err != nil {
			infraegress.RecordRouteRuleOutcome(scope, class, infraegress.RouteRuleOutcomeNodeUnavailable)
			return nil, false
		}
		infraegress.RecordRouteRuleOutcome(scope, class, infraegress.RouteRuleOutcomeHit)
		return lease, true
	}
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
			t.manager.FeedbackForScope(context.WithoutCancel(request.Context()), domainegress.ScopeBuild, lease.NodeID, 0, err)
		}
		released = true
		lease.Release()
		return nil, err
	}
	t.manager.FeedbackForScope(context.WithoutCancel(request.Context()), domainegress.ScopeBuild, lease.NodeID, response.StatusCode, nil)
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
}

func (b *egressResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return err
}
