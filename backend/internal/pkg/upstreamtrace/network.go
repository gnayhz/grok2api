package upstreamtrace

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http/httptrace"
	"os"
	"sync"
	"time"
)

type networkEvent struct {
	Stage            string `json:"stage"`
	Microseconds     int64  `json:"us"`
	Reused           bool   `json:"reused,omitempty"`
	IdleMicroseconds int64  `json:"idle_us,omitempty"`
	Protocol         string `json:"protocol,omitempty"`
	Failed           bool   `json:"failed,omitempty"`
}

// Network samples connection timing only when upstream tracing is enabled.
// No addresses, headers, credentials or request content enter this report.
// Finish is called after receiving headers; streaming arrival times are in the
// existing raw trace. Hooks compose with transport accounting and retry hooks.
func Network(ctx context.Context, provider, operation string) (context.Context, func()) {
	d, enabled := Enabled()
	if !enabled {
		return ctx, func() {}
	}
	start := time.Now()
	var mu sync.Mutex
	events := make([]networkEvent, 0, 12)
	closed := false
	note := func(event networkEvent) {
		mu.Lock()
		defer mu.Unlock()
		if closed || len(events) >= 128 {
			return
		}
		event.Microseconds = time.Since(start).Microseconds()
		events = append(events, event)
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { note(networkEvent{Stage: "get_conn"}) },
		GotConn: func(info httptrace.GotConnInfo) {
			note(networkEvent{Stage: "got_conn", Reused: info.Reused, IdleMicroseconds: info.IdleTime.Microseconds()})
		},
		DNSStart:          func(httptrace.DNSStartInfo) { note(networkEvent{Stage: "dns_start"}) },
		DNSDone:           func(info httptrace.DNSDoneInfo) { note(networkEvent{Stage: "dns_done", Failed: info.Err != nil}) },
		ConnectStart:      func(string, string) { note(networkEvent{Stage: "connect_start"}) },
		ConnectDone:       func(_ string, _ string, err error) { note(networkEvent{Stage: "connect_done", Failed: err != nil}) },
		TLSHandshakeStart: func() { note(networkEvent{Stage: "tls_start"}) },
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			note(networkEvent{Stage: "tls_done", Protocol: state.NegotiatedProtocol, Failed: err != nil})
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			note(networkEvent{Stage: "request_written", Failed: info.Err != nil})
		},
		GotFirstResponseByte: func() { note(networkEvent{Stage: "first_response_byte"}) },
	}
	return httptrace.WithClientTrace(ctx, trace), func() {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		closed = true
		data, err := json.MarshalIndent(struct {
			Provider   string         `json:"provider"`
			Operation  string         `json:"operation"`
			StartedUTC time.Time      `json:"started_utc"`
			Events     []networkEvent `json:"events"`
		}{provider, operation, start.UTC(), events}, "", "  ")
		if err == nil {
			_ = os.WriteFile(path(d, operation, provider, "network", "json"), data, 0600)
		}
	}
}
