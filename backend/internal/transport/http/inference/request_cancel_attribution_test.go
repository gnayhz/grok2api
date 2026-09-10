package inference

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// A server lifetime can end while the client is still consuming the response.
// Its canceled context alone cannot prove that the client disconnected.
func TestHTTPRequestLifetimeCancellationDoesNotClaimClientDisconnect(t *testing.T) {
	for _, provider := range []account.Provider{account.ProviderBuild, account.ProviderConsole} {
		t.Run(string(provider), func(t *testing.T) {
			for _, dialect := range []string{"sqlite", "postgres"} {
				for _, operation := range []string{"responses", "chat/completions", "messages"} {
					for _, cause := range []string{"server_cancel", "deadline", "complete"} {
						t.Run(dialect+"/"+operation+"/"+cause, func(t *testing.T) {
							var calls atomic.Int32
							upstreamClosed := make(chan struct{})
							f := newResponseRetentionFixture(t, dialect, provider, responseRetentionHooks{beforeResponse: func(w http.ResponseWriter, r *http.Request) bool {
								calls.Add(1)
								if cause == "complete" {
									close(upstreamClosed)
									return false
								}
								defer close(upstreamClosed)
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_server_cancel\",\"model\":\"grok-4.5\"}}\n\ndata: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"working through the request\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"partial answer\"}\n\n")
								w.(http.Flusher).Flush()
								<-r.Context().Done()
								return true
							}})
							f.gateway.UpdateQualityRetry(gateway.QualityRetryRuntime{Enabled: false, MaxAttempts: 1})
							serverCtx, cancelServer := context.WithCancel(context.Background())
							if cause == "deadline" {
								cancelServer()
								serverCtx, cancelServer = context.WithTimeout(context.Background(), 2*time.Second)
							}
							defer cancelServer()
							finished := make(chan struct{})
							server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(finished); f.router.ServeHTTP(w, r) }))
							server.Config.BaseContext = func(net.Listener) context.Context { return serverCtx }
							server.Start()
							defer server.Close()
							payload := map[string]any{"model": f.model, "stream": true, "input": "hello"}
							if operation != "responses" {
								delete(payload, "input")
								payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
							}
							if operation == "messages" {
								payload["max_tokens"] = 16
							}
							body, _ := json.Marshal(payload)
							req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, strings.NewReader(string(body)))
							if err != nil {
								t.Fatal(err)
							}
							req.Header.Set("Authorization", "Bearer "+f.secret)
							req.Header.Set("Content-Type", "application/json")
							req.Header.Set("Anthropic-Version", "2023-06-01")
							client := server.Client()
							client.Timeout = 8 * time.Second
							response, err := client.Do(req)
							if err != nil {
								t.Fatal(err)
							}
							defer response.Body.Close()
							reader := bufio.NewReader(response.Body)
							line, err := reader.ReadString('\n')
							if err != nil || response.StatusCode != 200 {
								t.Fatalf("response did not begin: status=%d err=%v", response.StatusCode, err)
							}
							if cause == "server_cancel" {
								cancelServer()
							}
							rest, err := io.ReadAll(reader)
							if err != nil {
								t.Fatalf("client remained connected but read failed: %v", err)
							}
							select {
							case <-finished:
							case <-time.After(5 * time.Second):
								t.Fatal("handler did not finish")
							}
							select {
							case <-upstreamClosed:
							case <-time.After(5 * time.Second):
								t.Fatal("upstream request was not released")
							}
							record := f.lastAudit(t)
							want := "request_canceled"
							if cause == "complete" {
								want = ""
							}
							if record.ErrorCode != want {
								t.Errorf("server cause=%s recorded client claim=%q, wanted %q; status=%d delivery=%s", cause, record.ErrorCode, want, record.StatusCode, record.DeliveryOutcome)
							}
							if calls.Load() != 1 {
								t.Fatalf("cancellation retried generation: %d", calls.Load())
							}
							if record.DeliveredBytes != int64(len(line)+len(rest)) || record.LedgerOutcome != "committed" {
								t.Errorf("delivery bytes or ledger lost: bytes=%d client=%d ledger=%s", record.DeliveredBytes, len(line)+len(rest), record.LedgerOutcome)
							}
							if cause != "complete" {
								if record.StatusCode != response.StatusCode {
									t.Errorf("server canceled request invented a client status: audit=%d actual=%d", record.StatusCode, response.StatusCode)
								}
								if record.DeliveryOutcome != "canceled" || record.OwnershipCommit == "committed" || record.HistoryCommit == "committed" {
									t.Errorf("canceled request committed success: delivery=%s ownership=%s history=%s", record.DeliveryOutcome, record.OwnershipCommit, record.HistoryCommit)
								}
								output := line + string(rest)
								for _, terminal := range []string{`"type":"response.completed"`, `"finish_reason":"stop"`, `"type":"message_stop"`} {
									if strings.Contains(output, terminal) {
										t.Errorf("canceled stream sent success: %s", terminal)
									}
								}
							} else if record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" {
								t.Errorf("normal completion changed: delivery=%s ledger=%s", record.DeliveryOutcome, record.LedgerOutcome)
							}
							t.Log(fmt.Sprintf("calls=%d code=%q delivery=%s", calls.Load(), record.ErrorCode, record.DeliveryOutcome))
						})
					}
				}
			}
		})
	}
}
