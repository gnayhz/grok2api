package egress

import (
	"context"
	"errors"
	"net/http"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// CooldownNodeForProbeFailure applies a transport cooldown to a node whose
// exit was confirmed dead by probes (both address families failing twice in
// a row). It mirrors the request-transport feedback branch so recovery
// semantics match exactly: UpdateEgressNodeProbe clears it on the next
// healthy probe, without any manual intervention.
func (m *Manager) CooldownNodeForProbeFailure(ctx context.Context, nodeID uint64, until time.Time) error {
	if m == nil || nodeID == 0 {
		return nil
	}
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return err
	}
	return m.CooldownBindingForProbeFailure(ctx, value, until)
}

func (m *Manager) CooldownBindingForProbeFailure(ctx context.Context, value domain.Node, until time.Time) error {
	_, err := m.repository.ApplyEgressHealthObservation(ctx, domain.HealthObservation{NodeID: value.ID, EncryptedProxyURL: value.EncryptedProxyURL, BindingRevision: value.BindingRevision, Kind: domain.HealthTransportFailure, ObservedAt: time.Now().UTC(), CooldownUntil: &until})
	if err == nil {
		m.invalidateNodes()
	}
	return err
}

// Feedback is the compatibility entry for observations without a lease.
// Providers use Lease.Observe so the original node binding and revision travel
// with the completion event. Both entries enqueue; neither waits for storage.
func (m *Manager) Feedback(ctx context.Context, nodeID uint64, status int, transportErr error) {
	m.FeedbackForScope(ctx, domain.ScopeWeb, nodeID, status, transportErr)
}

func feedbackKind(scope domain.Scope, status int, err error) (domain.HealthObservationKind, bool) {
	if runtimeCapacityError(err) {
		return 0, false
	}
	if status == clientClosedRequestStatus || errors.Is(err, context.Canceled) || neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
		return 0, false
	}
	if scope == domain.ScopeBuild && neterrorpkg.IsResponseHeaderTimeout(err) {
		return 0, false
	}
	if err != nil {
		return domain.HealthTransportFailure, true
	}
	if status >= 200 && status < 400 {
		return domain.HealthSuccess, true
	}
	if status == http.StatusForbidden && scope != domain.ScopeBuild && scope != domain.ScopeConsoleAsset {
		return domain.HealthAntiBotRejection, true
	}
	return 0, false
}

func (m *Manager) FeedbackForScope(_ context.Context, scope domain.Scope, nodeID uint64, status int, transportErr error) {
	kind, ok := feedbackKind(scope, status, transportErr)
	if kind == domain.HealthSuccess && m.cachedNodeIsHealthy(nodeID) {
		return
	}
	if !ok || (nodeID == 0 && kind == domain.HealthSuccess) {
		return
	}
	m.health.submit(healthReport{binding: healthBinding{nodeID: nodeID}, scope: scope, observation: domain.HealthObservation{NodeID: nodeID, Kind: kind, Failures: 1, ObservedAt: time.Now().UTC()}})
}

// Observe reports a classified request or stream completion without waiting
// for persistence. The immutable lease binding prevents late completions from
// changing a newly configured proxy. Providers retain response-body ownership.
func (l *Lease) Observe(status int, transportErr error) {
	if l == nil || l.clearanceManager == nil {
		return
	}
	// Only errors produced at the physical transport boundary can cool a node.
	// A decoder failure, model error or downstream disconnect says nothing
	// about the selected proxy's health.
	if transportErr != nil && !neterrorpkg.IsTransport(transportErr) {
		return
	}
	// A rejected HTTP handshake has reached the server. Classify its response
	// before the WebSocket library's generic bad-handshake error.
	if status == http.StatusForbidden {
		transportErr = nil
	}
	kind, ok := feedbackKind(l.Scope, status, transportErr)
	if !ok || (l.NodeID == 0 && kind == domain.HealthSuccess) {
		return
	}
	if kind == domain.HealthAntiBotRejection {
		if l.clearanceKey != "" {
			if !l.clearanceManager.clearance.invalidateClearanceKey(l.clearanceKey, l.client, l.clearanceGeneration) {
				// A late rejection of an older browser session is not evidence
				// against its replacement, nor a reason to retire its clients.
				return
			}
		} else if l.client != nil {
			l.client.CloseIdleConnections()
		}
	}
	l.clearanceManager.health.submit(healthReport{
		binding: healthBinding{l.NodeID, l.healthProxy, l.healthBindingRevision}, baseline: l.healthBaseline, scope: l.Scope, known: true, proxyPool: l.proxyPool,
		observation: domain.HealthObservation{NodeID: l.NodeID, EncryptedProxyURL: l.healthProxy, BindingRevision: l.healthBindingRevision, ExpectedRevision: l.healthBaseline.Revision, Kind: kind, Failures: 1, ObservedAt: time.Now().UTC()},
	})
}

func (m *Manager) persistHealthReport(ctx context.Context, r healthReport) (domain.HealthState, error) {
	if r.binding.nodeID == 0 {
		m.invalidateObservedClients(r)
		return r.baseline, nil
	}
	var node domain.Node
	if !r.known {
		var err error
		node, err = m.getRuntimeNode(ctx, r.binding.nodeID)
		if err != nil {
			return domain.HealthState{}, err
		}
		r.baseline = node.HealthState()
		r.proxyPool = m.isProxyPoolNode(node)
		r.observation.EncryptedProxyURL = node.EncryptedProxyURL
		r.observation.ExpectedRevision = node.HealthRevision
		r.observation.BindingRevision = node.BindingRevision
	}
	if r.observation.Kind != domain.HealthSuccess {
		recordPoolNodeFailures(r.binding.nodeID, r.observation.Failures)
	}
	if r.proxyPool && r.observation.Kind != domain.HealthSuccess {
		return r.baseline, nil
	}
	if r.observation.Kind == domain.HealthSuccess && healthStateHealthy(r.baseline) {
		return r.baseline, nil
	}
	if !r.known && r.observation.Kind != domain.HealthSuccess {
		m.invalidateObservedClients(r)
	}
	state, err := m.repository.ApplyEgressHealthObservation(ctx, r.observation)
	if err != nil {
		return domain.HealthState{}, err
	}

	if r.observation.Kind != domain.HealthSuccess {
		if r.known {
			m.invalidateObservedClients(r)
		}
		if r.observation.Kind == domain.HealthTransportFailure {
			if node.ID == 0 {
				node = domain.Node{ID: r.binding.nodeID, EncryptedProxyURL: r.observation.EncryptedProxyURL}
			}
			m.scheduleFailureProbe(node)
		}
	}
	return state, nil
}

func (m *Manager) invalidateObservedClients(r healthReport) {
	nodeID, scope, kind := r.binding.nodeID, r.scope, r.observation.Kind
	if r.known && kind == domain.HealthAntiBotRejection {
		// Lease.Observe has already applied the exact session generation.
		// A queued health write must never invalidate a later solve/client.
		return
	}
	if kind == domain.HealthAntiBotRejection {
		m.clearance.observeRejection(nodeID, scope)
	}
	if kind != domain.HealthAntiBotRejection {
		scope = ""
	}
	m.transport.invalidate(map[uint64]struct{}{nodeID: {}}, scope)
}

func nodeIsHealthy(value domain.Node) bool {
	return healthStateHealthy(value.HealthState())
}

// ObserveWebSocketError is used only for errors returned directly by upstream
// WebSocket reads/writes. Parsed model errors continue through Observe.
func (l *Lease) ObserveWebSocketError(err error) {
	l.Observe(0, neterrorpkg.MarkTransport(err, neterrorpkg.PhaseWebSocket))
}
