package inference

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// The response encoder owns buffered bytes after Write has accepted the JSON.
// Its body/trailer failure must reach delivery finalization and its actual byte
// count, while retaining the generation, necessary commits and ledger facts.
func TestHTTPResponseEncodingFinishesBeforeDeliveryRecord(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderWeb, account.ProviderConsole} {
			for _, operation := range []string{"responses", "chat/completions", "messages"} {
				for _, failure := range []string{"none", "body", "trailer"} {
					t.Run(dialect+"/"+string(kind)+"/"+operation+"/"+failure, func(t *testing.T) {
						f := newResponseRetentionFixture(t, dialect, kind, responseRetentionHooks{middleware: []gin.HandlerFunc{middleware.Gzip()}})
						done := make(chan struct{})
						var output *encodingFailureWriter
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							defer close(done)
							output = &encodingFailureWriter{ResponseWriter: w, failure: failure}
							f.router.ServeHTTP(output, r)
						}))
						defer server.Close()
						payload := map[string]any{"model": f.model, "stream": false, "input": "hello"}
						if operation != "responses" {
							delete(payload, "input")
							payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
						}
						if operation == "messages" {
							payload["max_tokens"] = 16
						}
						body, _ := json.Marshal(payload)
						req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, bytes.NewReader(body))
						if err != nil {
							t.Fatal(err)
						}
						req.Header.Set("Authorization", "Bearer "+f.secret)
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Accept-Encoding", "gzip")
						req.Header.Set("Anthropic-Version", "2023-06-01")
						req.Header.Set("X-Session-Id", "encoding-lifetime")
						client := server.Client()
						client.Timeout = 5 * time.Second
						resp, err := client.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						wire, readErr := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if readErr != nil {
							t.Fatal(readErr)
						}
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Fatal("HTTP handler did not finish")
						}
						if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" {
							t.Fatalf("status=%d encoding=%s", resp.StatusCode, resp.Header.Get("Content-Encoding"))
						}
						record := f.lastAudit(t)
						if record.GenerationOutcome != "completed" || record.LedgerOutcome != "committed" || f.generated.Load() != 1 || record.InputTokens <= 0 || record.OutputTokens <= 0 {
							t.Errorf("generation or ledger changed: generation=%s ledger=%s calls=%d usage=%d/%d", record.GenerationOutcome, record.LedgerOutcome, f.generated.Load(), record.InputTokens, record.OutputTokens)
						}
						wantOwner, wantState, wantHistory := "not_required", "not_required", "not_required"
						if operation == "responses" && kind != account.ProviderConsole {
							wantOwner = "committed"
						}
						if operation == "responses" && kind == account.ProviderWeb {
							wantState = "committed"
						}
						if kind == account.ProviderBuild {
							wantHistory = "committed"
						}
						if record.OwnershipCommit != wantOwner || record.ProviderStateCommit != wantState || record.HistoryCommit != wantHistory || record.PhysicalReceipt != "committed" {
							t.Errorf("encoding failure changed necessary commits: ownership=%s state=%s history=%s physical=%s", record.OwnershipCommit, record.ProviderStateCommit, record.HistoryCommit, record.PhysicalReceipt)
						}
						if record.DeliveredBytes != int64(len(wire)) || output.accepted != len(wire) {
							t.Errorf("recorded before encoder drained: audit=%d writer=%d wire=%d", record.DeliveredBytes, output.accepted, len(wire))
						}
						zr, err := gzip.NewReader(bytes.NewReader(wire))
						if err != nil {
							t.Fatal(err)
						}
						decoded, decodeErr := io.ReadAll(zr)
						_ = zr.Close()
						if failure == "none" {
							if decodeErr != nil || !json.Valid(decoded) || record.ErrorCode != "" || record.DeliveryOutcome != "completed" || output.failed {
								t.Errorf("ordinary compressed result: decode=%v code=%s delivery=%s failed=%v", decodeErr, record.ErrorCode, record.DeliveryOutcome, output.failed)
							}
						} else if !output.failed || decodeErr == nil || record.ErrorCode != "client_disconnected" || record.DeliveryOutcome != "canceled" {
							t.Errorf("encoder failure lost: injected=%v decode=%v code=%q delivery=%s", output.failed, decodeErr, record.ErrorCode, record.DeliveryOutcome)
						}
					})
				}
			}
		}
	}
}

type encodingFailureWriter struct {
	http.ResponseWriter
	failure  string
	accepted int
	failed   bool
}

func (w *encodingFailureWriter) Write(p []byte) (int, error) {
	if (w.failure == "body" && w.accepted >= 10) || (w.failure == "trailer" && len(p) == 8 && w.accepted > 10) {
		w.failed = true
		return 0, errors.New("injected encoded response write failure")
	}
	n, err := w.ResponseWriter.Write(p)
	w.accepted += n
	return n, err
}

func (w *encodingFailureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
