package inference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestHTTPDPoPSharedExchangeRequestLifetimes(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, streaming := range []bool{false, true} {
			for _, mode := range []string{"waiter_deadline", "owner_cancel"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", dialect, streaming, mode), func(t *testing.T) {
					entered, release := make(chan struct{}), make(chan struct{})
					var releaseOnce sync.Once
					defer releaseOnce.Do(func() { close(release) })
					var tokenCalls atomic.Int32
					f := newResponseRetentionFixture(t, dialect, account.ProviderConsole, responseRetentionHooks{beforeToken: func(r *http.Request) {
						if tokenCalls.Add(1) == 1 {
							close(entered)
							select {
							case <-release:
							case <-r.Context().Done():
							}
						}
					}})
					serve := func(ctx context.Context, name string) *httptest.ResponseRecorder {
						req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","store":false,"stream":%t}`, f.model, streaming))).WithContext(ctx)
						req.Header.Set("Authorization", "Bearer "+f.secret)
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("X-Request-ID", name)
						out := httptest.NewRecorder()
						f.router.ServeHTTP(out, req)
						return out
					}
					ownerCtx, cancelOwner := context.WithCancel(context.Background())
					defer cancelOwner()
					owner := make(chan *httptest.ResponseRecorder, 1)
					go func() { owner <- serve(ownerCtx, "dpop-owner") }()
					select {
					case <-entered:
					case <-time.After(2 * time.Second):
						t.Fatal("owner did not reach real mint")
					}
					var canceled, survived *httptest.ResponseRecorder
					timely := false
					if mode == "waiter_deadline" {
						waiterCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
						defer cancel()
						waiter := make(chan *httptest.ResponseRecorder, 1)
						go func() { waiter <- serve(waiterCtx, "dpop-waiter") }()
						select {
						case canceled = <-waiter:
							timely = true
						case <-time.After(800 * time.Millisecond):
						}
						if timely && f.generated.Load() != 0 {
							t.Error("generation escaped blocked owner")
						}
						releaseOnce.Do(func() { close(release) })
						if !timely {
							canceled = <-waiter
						}
						survived = <-owner
					} else {
						live := make(chan *httptest.ResponseRecorder, 1)
						started := make(chan struct{})
						go func() { close(started); live <- serve(context.Background(), "dpop-waiter") }()
						<-started
						time.Sleep(100 * time.Millisecond)
						cancelOwner()
						select {
						case canceled = <-owner:
							timely = true
						case <-time.After(800 * time.Millisecond):
						}
						releaseOnce.Do(func() { close(release) })
						if !timely {
							canceled = <-owner
						}
						select {
						case survived = <-live:
						case <-time.After(3 * time.Second):
							t.Fatal("live waiter did not continue")
						}
					}
					if !timely || canceled.Code != 502 || !strings.Contains(canceled.Body.String(), "upstream_unavailable") || survived.Code != 200 {
						t.Fatalf("request lifecycle timely=%t canceled=%d/%s survivor=%d/%s", timely, canceled.Code, canceled.Body.String(), survived.Code, survived.Body.String())
					}
					retentionResponse(t, survived, streaming)
					wantTokens := int32(1)
					if mode == "owner_cancel" {
						wantTokens = 2
					}
					if tokenCalls.Load() != wantTokens || f.generated.Load() != 1 || f.states.stateCalls.Load() != 0 || f.states.ownershipCalls.Load() != 0 {
						t.Fatalf("token=%d generated=%d native=%d owner=%d", tokenCalls.Load(), f.generated.Load(), f.states.stateCalls.Load(), f.states.ownershipCalls.Load())
					}
					records, count, err := f.audits.List(context.Background(), 0, 10)
					if err != nil || count != 2 {
						t.Fatalf("audit count=%d err=%v", count, err)
					}
					completed, canceledRecords := 0, 0
					for _, value := range records {
						record, err := f.audits.Get(context.Background(), value.ID)
						if err != nil {
							t.Fatal(err)
						}
						if record.ErrorCode == "request_canceled" {
							canceledRecords++
							if record.GenerationOutcome == "completed" || record.InputTokens != 0 || record.OutputTokens != 0 || len(record.GenerationUsages) != 0 {
								t.Fatalf("canceled exchange charged inference: %+v", record)
							}
						} else {
							completed++
							if record.ErrorCode != "" || record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" || len(record.GenerationUsages) != 1 || record.InputTokens != 20 || record.OutputTokens != 5 {
								t.Fatalf("survivor facts lost: %+v", record)
							}
						}
					}
					if completed != 1 || canceledRecords != 1 {
						t.Fatalf("completed=%d canceled=%d", completed, canceledRecords)
					}
					followup := serve(context.Background(), "dpop-followup")
					retentionResponse(t, followup, streaming)
					if tokenCalls.Load() != wantTokens || f.generated.Load() != 2 {
						t.Fatalf("followup did not share valid cache: mint=%d generated=%d", tokenCalls.Load(), f.generated.Load())
					}
				})
			}
		}
	}
}
