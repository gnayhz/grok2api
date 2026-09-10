package app

import (
	"context"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	qualityproxy "github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

// qualityProxyDialer delegates route, pool and connection decisions to Manager
// and records successful acquisitions for the quality distribution view.
type qualityProxyDialer struct {
	manager *infraegress.Manager
	policy  *qualityproxy.DialerPolicy
}

var _ infraegress.Dialer = qualityProxyDialer{}

func (d qualityProxyDialer) AcquireIfConfigured(ctx context.Context, scope domainegress.Scope, affinity string) (*infraegress.Lease, bool, error) {
	lease, configured, err := d.manager.AcquireIfConfigured(ctx, scope, affinity)
	if err == nil && configured && lease != nil {
		d.policy.ObserveAcquisition(string(scope), lease.NodeID)
	}
	return lease, configured, err
}

func (d qualityProxyDialer) AcquireBuildEnvironmentDirectIfIsolated(ctx context.Context, affinity string) (*infraegress.Lease, bool, error) {
	return d.manager.AcquireBuildEnvironmentDirectIfIsolated(ctx, affinity)
}

func (d qualityProxyDialer) AcquireBuildEnvironmentDirect(ctx context.Context, affinity string) (*infraegress.Lease, error) {
	return d.manager.AcquireBuildEnvironmentDirect(ctx, affinity)
}

func (d qualityProxyDialer) FeedbackForScope(ctx context.Context, scope domainegress.Scope, nodeID uint64, statusCode int, err error) {
	d.manager.FeedbackForScope(ctx, scope, nodeID, statusCode, err)
}

func (d qualityProxyDialer) BuildStreamIdleTimeout() time.Duration {
	return d.manager.BuildStreamIdleTimeout()
}
