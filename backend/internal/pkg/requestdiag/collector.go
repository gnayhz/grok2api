// Package requestdiag supplies bounded, content-free request observations.
// It has no routing, retry, completion or billing authority.
package requestdiag

import (
	"context"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

type contextKey struct{}
type Collector struct {
	mu     sync.Mutex
	report audit.ExecutionDiagnostics
}

func WithCollector(ctx context.Context) context.Context {
	if FromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, &Collector{report: audit.ExecutionDiagnostics{Version: 1}})
}
func FromContext(ctx context.Context) *Collector {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(contextKey{}).(*Collector)
	return c
}
func Stage(ctx context.Context, stage string, started time.Time) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.report.Stages {
		if c.report.Stages[i].Stage == stage {
			c.report.Stages[i].US += max(0, time.Since(started).Microseconds())
			c.report.Stages[i].Calls++
			return
		}
	}
	if len(c.report.Stages) >= 32 {
		c.report.Truncated = true
		return
	}
	c.report.Stages = append(c.report.Stages, audit.StageDiagnostic{Stage: stage, US: max(0, time.Since(started).Microseconds()), Calls: 1})
}
func Failure(ctx context.Context, component, stage, reason string) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.report.Failures) >= 8 {
		c.report.Truncated = true
		return
	}
	c.report.Failures = append(c.report.Failures, audit.FailureDiagnostic{Component: component, Stage: stage, Reason: reason})
}
func (c *Collector) exchange(v audit.ExchangeDiagnostic) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.report.Exchanges) >= 32 {
		c.report.Truncated = true
		return
	}
	c.report.Exchanges = append(c.report.Exchanges, v)
}
func Snapshot(ctx context.Context) *audit.ExecutionDiagnostics {
	c := FromContext(ctx)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.report
	v.Stages = append([]audit.StageDiagnostic(nil), v.Stages...)
	v.Failures = append([]audit.FailureDiagnostic(nil), v.Failures...)
	// Exchange data is immutable after publication.
	v.Exchanges = append([]audit.ExchangeDiagnostic(nil), v.Exchanges...)
	return &v
}
