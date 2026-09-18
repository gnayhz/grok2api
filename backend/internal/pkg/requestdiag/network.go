package requestdiag

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// Network observes one physical submission through receipt of response headers.
// This cannot separate upstream queueing, prefill and generation. No endpoint,
// connection address, headers or error text is retained.
func Network(ctx context.Context, plane, stage string) (context.Context, func(int, error)) {
	c := FromContext(ctx)
	if c == nil {
		return ctx, func(int, error) {}
	}
	id := attemptmeta.FromContext(ctx)
	if id.ID == "" {
		return ctx, func(int, error) {}
	}
	start := time.Now()
	var mu sync.Mutex
	closed := false
	truncated := false
	prompt, _ := ctx.Value(promptKeyContext{}).(*audit.PromptDiagnostic)
	v := audit.ExchangeDiagnostic{PhysicalID: id.ID, AccountID: id.AccountID, NodeID: id.Path.NodeID, Epoch: id.Path.Epoch, Plane: plane, Stage: stage, Prompt: prompt}
	note := func(stage string, reused, resumed, failed bool) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		if len(v.Events) >= 32 {
			truncated = true
			return
		}
		v.Events = append(v.Events, audit.NetworkDiagnostic{Stage: stage, US: time.Since(start).Microseconds(), Reused: reused, Resumed: resumed, Failed: failed})
	}
	trace := &httptrace.ClientTrace{
		GetConn:              func(string) { note("get_conn", false, false, false) },
		GotConn:              func(i httptrace.GotConnInfo) { note("got_conn", i.Reused, false, false) },
		DNSStart:             func(httptrace.DNSStartInfo) { note("dns_start", false, false, false) },
		DNSDone:              func(i httptrace.DNSDoneInfo) { note("dns_done", false, false, i.Err != nil) },
		ConnectStart:         func(string, string) { note("connect_start", false, false, false) },
		ConnectDone:          func(_ string, _ string, e error) { note("connect_done", false, false, e != nil) },
		TLSHandshakeStart:    func() { note("tls_start", false, false, false) },
		TLSHandshakeDone:     func(s tls.ConnectionState, e error) { note("tls_done", false, s.DidResume, e != nil) },
		WroteRequest:         func(i httptrace.WroteRequestInfo) { note("request_written", false, false, i.Err != nil) },
		GotFirstResponseByte: func() { note("first_response_byte", false, false, false) },
	}
	return httptrace.WithClientTrace(ctx, trace), func(status int, err error) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		closed = true
		v.Status, v.Failed, v.US = status, err != nil, time.Since(start).Microseconds()
		if truncated {
			c.mu.Lock()
			c.report.Truncated = true
			c.mu.Unlock()
		}
		c.exchange(v)
	}
}
