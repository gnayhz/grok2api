package egress

import "context"

type selectionVersionKey struct{}

// A selected node and any clearance/client construction that follows it share
// one configuration generation. Invalidation finishes before advancing this
// generation, so an old selection cannot become a newly published lease.
func (r *routingRuntime) selectionContext(ctx context.Context) context.Context {
	if _, ok := ctx.Value(selectionVersionKey{}).(uint64); ok {
		return ctx
	}
	return context.WithValue(ctx, selectionVersionKey{}, r.bindingGeneration.Load())
}
func (r *routingRuntime) selectionCurrent(ctx context.Context) bool {
	version, ok := ctx.Value(selectionVersionKey{}).(uint64)
	return !ok || version == r.bindingGeneration.Load()
}
func (m *Manager) invalidateBindings() { m.routing.bindingGeneration.Add(1) }
