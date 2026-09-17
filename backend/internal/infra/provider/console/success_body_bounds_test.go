package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestConsoleSuccessfulDocumentsRequireCompleteBodies(t *testing.T) {
	for _, endpoint := range []string{"token", "quota"} {
		for _, mode := range []string{"normal", "exact_limit", "over_whitespace", "over_broken", "read_error", "canceled"} {
			t.Run(endpoint+"/"+mode, func(t *testing.T) {
				valid := mode == "normal" || mode == "exact_limit"
				var calls atomic.Int32
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				writeBody := func(w http.ResponseWriter, r *http.Request, body string, affected bool) {
					w.Header().Set("Content-Type", "application/json")
					if affected {
						switch mode {
						case "exact_limit":
							body += strings.Repeat(" ", provider.MaxDiagnosticBodyBytes-len(body))
						case "over_whitespace":
							body += strings.Repeat(" ", provider.MaxDiagnosticBodyBytes+1-len(body))
						case "over_broken":
							body += strings.Repeat(" ", provider.MaxDiagnosticBodyBytes-len(body)) + "{broken tail"
						case "read_error":
							w.Header().Set("Content-Length", strconv.Itoa(len(body)+7))
						}
					}
					_, _ = io.WriteString(w, body)
					if affected && mode == "canceled" {
						w.(http.Flusher).Flush()
						cancel()
						<-r.Context().Done()
					}
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/dpop/token" {
						record := httptest.NewRecorder()
						serveTestDPoPToken(t, record, r)
						writeBody(w, r, record.Body.String(), endpoint == "token")
						return
					}
					calls.Add(1)
					verifyTestDPoPProof(t, r)
					if endpoint == "quota" {
						writeBody(w, r, `{"quotas":[{"kind":"chat","limit":10,"used":1,"remaining":9},{"kind":"image","limit":5,"used":0,"remaining":5},{"kind":"video","limit":2,"used":0,"remaining":2}]}`, true)
						return
					}
					writeBody(w, r, `{"id":"resp_body","object":"response","status":"completed","output":[]}`, false)
				}))
				defer server.Close()
				adapter, credential := newConsoleTestAdapter(t, server.URL)
				var err error
				if endpoint == "quota" {
					var result provider.QuotaSnapshot
					result, err = adapter.SyncQuota(ctx, credential)
					if !valid && len(result.Windows) != 0 {
						t.Errorf("incomplete body published %d quota windows", len(result.Windows))
					}
					if valid && len(result.Windows) != 3 {
						t.Fatalf("normal quota: %v", err)
					}
				} else {
					var response *provider.Response
					response, err = adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3", Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`)})
					if response != nil {
						_, _ = io.Copy(io.Discard, response.Body)
						_ = response.Body.Close()
					}
					if !valid && calls.Load() != 0 {
						t.Errorf("incomplete token authorized %d inference calls", calls.Load())
					}
				}
				if !valid && err == nil {
					t.Fatal("incomplete successful document was accepted")
				}
				if valid && err != nil {
					t.Fatal(err)
				}
				if mode == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				if mode == "read_error" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("lost read failure: %v", err)
				}
				if strings.HasPrefix(mode, "over_") && !strings.Contains(fmt.Sprint(err), "64 KiB") {
					t.Fatalf("missing size failure: %v", err)
				}
			})
		}
	}
}
