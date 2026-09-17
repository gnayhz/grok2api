// Package netfetch exposes the infra subscription transport to application-level
// sync tests. It lives apart from testsupport's root because that package is
// imported by infra/egress tests, and a helper importing infra/egress from
// there would form an import cycle.
package netfetch

import (
	"context"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

// NewEgressSubscriptionFetcher exposes the infra subscription transport so
// application-level sync integration tests drive the real fetch contract
// without importing infrastructure packages directly.
func NewEgressSubscriptionFetcher(owner infraegress.ControlTransportOwner, normalize func(string) (string, error)) interface {
	FetchProxySubscription(ctx context.Context, url, viaProxy string) ([]byte, error)
} {
	return infraegress.NewSubscriptionFetcher(owner, normalize)
}
