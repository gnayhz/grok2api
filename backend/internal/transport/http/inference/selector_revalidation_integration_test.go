package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Only the point between ordering and the current SQL read is controlled. The
// change itself uses M07 and all routing/material/slot operations are real.
type httpCurrentRoutingFacts struct {
	repository.AccountRepository
	before func(context.Context, uint64) error
	reads  atomic.Int32
}

func (r *httpCurrentRoutingFacts) GetRoutingCandidate(ctx context.Context, id uint64, provider account.Provider, route uint64, model, mode string) (account.RoutingCandidate, error) {
	r.reads.Add(1)
	if r.before != nil {
		before := r.before
		r.before = nil
		if err := before(ctx, id); err != nil {
			return account.RoutingCandidate{}, err
		}
	}
	return r.AccountRepository.GetRoutingCandidate(ctx, id, provider, route, model, mode)
}

func TestHTTPSelectionRechecksCommittedHardFacts(t *testing.T) {
	for _, operation := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, restriction := range []string{"risk", "tier_scope"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", operation, stream, restriction), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					var generated atomic.Int32
					upstream := completionHTTPUpstream(t, "grok-4.5", &generated)
					defer upstream.Close()
					var port *httpCurrentRoutingFacts
					fx := newProviderCompletionFixtureWithAccountPorts(t, upstream.URL, "grok-4.5", account.ProviderBuild, nil, nil, nil, func(repo repository.AccountRepository) repository.AccountRepository {
						port = &httpCurrentRoutingFacts{AccountRepository: repo}
						return port
					})
					if err := fx.accounts.UpdateObservedModel(ctx, fx.account.ID, "grok-4.5-build-free", time.Now()); err != nil {
						t.Fatal(err)
					}
					providerScope, tierScope := clientkey.ProviderScopeBuild, clientkey.TierScopeFree
					if _, err := fx.clients.Patch(ctx, fx.created.Key.ID, clientkey.ManagementPatch{ProviderScope: &providerScope, TierScope: &tierScope}); err != nil {
						t.Fatal(err)
					}
					port.before = func(ctx context.Context, id uint64) error {
						patch := repository.AccountAdminPatch{Risk: &repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: "manual"}}
						if restriction == "tier_scope" {
							enabled := true
							patch = repository.AccountAdminPatch{BuildSuperEntitled: &enabled}
						}
						_, err := fx.accounts.UpdateAdministration(ctx, id, patch)
						return err
					}
					finished := make(chan struct{}, 2)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer func() { finished <- struct{}{} }()
						fx.router.ServeHTTP(w, r)
					}))
					defer server.Close()
					payload := map[string]any{"model": fx.publicModel, "stream": stream}
					if operation == "responses" {
						payload["input"] = "synthetic hard eligibility"
					} else {
						payload["messages"] = []any{map[string]any{"role": "user", "content": "synthetic hard eligibility"}}
					}
					if operation == "messages" {
						payload["max_tokens"] = 128
					}
					data, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					request := func(name string) (int, string) {
						t.Helper()
						req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/"+operation, bytes.NewReader(data))
						if err != nil {
							t.Fatal(err)
						}
						req.Header.Set("Authorization", "Bearer "+fx.created.Secret)
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("anthropic-version", "2023-06-01")
						req.Header.Set("X-Request-ID", name)
						res, err := server.Client().Do(req)
						if err != nil {
							t.Fatal(err)
						}
						body, err := io.ReadAll(res.Body)
						res.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						select {
						case <-finished:
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
						return res.StatusCode, string(body)
					}
					status, body := request("g19-refused")
					if status != http.StatusServiceUnavailable || !strings.Contains(body, "client_key_account_scope_unavailable") {
						t.Fatalf("claim refusal: status=%d body=%s", status, body)
					}
					if generated.Load() != 0 || port.reads.Load() != 1 {
						t.Fatalf("denied account attempted upstream: generation=%d reads=%d", generated.Load(), port.reads.Load())
					}
					records, count, err := fx.audits.List(ctx, 0, 10)
					if err != nil || count != 1 {
						t.Fatalf("refused audit count=%d err=%v", count, err)
					}
					record, err := fx.audits.Get(ctx, records[0].ID)
					if err != nil {
						t.Fatal(err)
					}
					if record.AttemptCount != 0 || len(record.GenerationUsages) != 0 || record.CostInUSDTicks != 0 || record.EstimatedCostInUSDTicks != 0 || record.AccountID != nil {
						t.Fatal("pre-attempt refusal recorded account/generation/cost")
					}
					key, err := fx.clients.Get(ctx, fx.created.Key.ID)
					if err != nil || key.BilledUsageUSDTicks != 0 || key.ReservedUsageUSDTicks != 0 {
						t.Fatalf("refused request retained billing: billed=%d reserved=%d err=%v", key.BilledUsageUSDTicks, key.ReservedUsageUSDTicks, err)
					}
					for _, key := range []string{repository.AccountConcurrencyKey(fx.account.ID), fmt.Sprintf("client:%d", fx.created.Key.ID)} {
						if count, err := fx.concurrency.Current(ctx, key); err != nil || count != 0 {
							t.Fatalf("refusal leaked capacity: count=%d err=%v", count, err)
						}
					}
					entitled := false
					if _, err := fx.accounts.UpdateAdministration(ctx, fx.account.ID, repository.AccountAdminPatch{Risk: &repository.RiskAttribution{}, BuildSuperEntitled: &entitled}); err != nil {
						t.Fatal(err)
					}
					status, body = request("g19-restored")
					if status != 200 || !strings.Contains(body, "completion answer") || generated.Load() != 1 {
						t.Fatalf("restored request failed: status=%d generated=%d body=%s", status, generated.Load(), body)
					}
				})
			}
		}
	}
}

func TestVideoWorkerRechecksCurrentFactsWithAcceptedScope(t *testing.T) {
	for _, restriction := range []string{"risk", "tier_scope"} {
		t.Run(restriction, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var calls, transport atomic.Int32
			upstream := voiceCompletionUpstream(t, &transport, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if r.URL.Path == "/rest/app-chat/conversations/new" {
					calls.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			})
			defer upstream.Close()
			var port *httpCurrentRoutingFacts
			fx := newProviderCompletionFixtureWithAccountPorts(t, upstream.URL, "grok-imagine-video", account.ProviderWeb, nil, nil, nil, func(repo repository.AccountRepository) repository.AccountRepository {
				port = &httpCurrentRoutingFacts{AccountRepository: repo}
				return port
			})
			providerScope, tierScope := clientkey.ProviderScopeWeb, clientkey.TierScopeFree
			if _, err := fx.clients.Patch(ctx, fx.created.Key.ID, clientkey.ManagementPatch{ProviderScope: &providerScope, TierScope: &tierScope}); err != nil {
				t.Fatal(err)
			}
			fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
			job := createVideoAuthorizationJob(t, fx)
			// A later key change cannot broaden this already accepted job.
			tierScope = clientkey.TierScopeAll
			if _, err := fx.clients.Patch(ctx, fx.created.Key.ID, clientkey.ManagementPatch{TierScope: &tierScope}); err != nil {
				t.Fatal(err)
			}
			port.before = func(ctx context.Context, id uint64) error {
				if restriction == "risk" {
					_, err := fx.accounts.UpdateAdministration(ctx, id, repository.AccountAdminPatch{Risk: &repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: "manual"}})
					return err
				}
				revision, err := fx.accounts.GetQuotaRevision(ctx, id)
				if err != nil {
					return err
				}
				return fx.accounts.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: id, Revision: revision, Tier: account.WebTierSuper, SyncedAt: time.Now()})
			}
			// Exercise durable queue recovery, retaining the warm candidate view.
			fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
			if err := fx.service.RecoverVideoJobs(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			for {
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, fx.created.Key.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.UsageRecordedAt != nil {
					job = stored
					break
				}
				select {
				case <-time.After(time.Millisecond):
				case <-ctx.Done():
					t.Fatal("worker did not finish")
				}
			}
			cancel()
			<-done
			if job.Status != media.StatusFailed || calls.Load() != 0 {
				t.Fatalf("invalid worker claim generated: status=%s calls=%d", job.Status, calls.Load())
			}
			scope, err := job.AccessPolicy.Scope()
			if err != nil || scope.Providers != clientkey.ProviderScopeWeb || scope.Tiers != clientkey.TierScopeFree {
				t.Fatal("accepted job scope changed")
			}
			records, count, err := fx.audits.List(context.Background(), 0, 10)
			if err != nil || count != 1 {
				t.Fatalf("video audit count=%d err=%v", count, err)
			}
			record, err := fx.audits.Get(context.Background(), records[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.AttemptCount != 0 || len(record.GenerationUsages) != 0 || record.CostInUSDTicks != 0 || record.EstimatedCostInUSDTicks != 0 {
				t.Fatal("refused video attempt recorded generation or cost")
			}
			key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
			if err != nil || key.ReservedUsageUSDTicks != 0 || key.BilledUsageUSDTicks != 0 {
				t.Fatal("refused video retained billing")
			}
			for _, key := range []string{repository.AccountConcurrencyKey(fx.account.ID), fmt.Sprintf("client:%d", fx.created.Key.ID)} {
				if count, err := fx.concurrency.Current(context.Background(), key); err != nil || count != 0 {
					t.Fatalf("video leaked capacity: %d %v", count, err)
				}
			}
		})
	}
}

type httpCurrentReadGate struct {
	repository.AccountRepository
	*httpSelectorReadPause
}

func (r *httpCurrentReadGate) GetRoutingCandidate(ctx context.Context, id uint64, provider account.Provider, route uint64, model, mode string) (account.RoutingCandidate, error) {
	value, err := r.AccountRepository.GetRoutingCandidate(ctx, id, provider, route, model, mode)
	return value, r.after(ctx, err)
}

func TestHTTPSelectorCurrentReadDisconnect(t *testing.T) {
	for _, operation := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", operation, stream), func(t *testing.T) {
				ctx := context.Background()
				var generated atomic.Int32
				upstream := completionHTTPUpstream(t, "grok-4.5", &generated)
				defer upstream.Close()
				pause := &httpSelectorReadPause{entered: make(chan struct{}), resume: make(chan struct{})}
				fx := newProviderCompletionFixtureWithAccountPorts(t, upstream.URL, "grok-4.5", account.ProviderBuild, nil, nil, nil, func(repo repository.AccountRepository) repository.AccountRepository {
					return &httpCurrentReadGate{AccountRepository: repo, httpSelectorReadPause: pause}
				})
				finished := make(chan struct{}, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer func() { finished <- struct{}{} }()
					fx.router.ServeHTTP(w, r)
				}))
				defer server.Close()
				defer close(pause.resume)
				payload := map[string]any{"model": fx.publicModel, "stream": stream}
				if operation == "responses" {
					payload["input"] = "synthetic current read cancellation"
				} else {
					payload["messages"] = []any{map[string]any{"role": "user", "content": "synthetic current read cancellation"}}
				}
				if operation == "messages" {
					payload["max_tokens"] = 128
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				requestCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL+"/v1/"+operation, bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+fx.created.Secret)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("anthropic-version", "2023-06-01")
				done := make(chan error, 1)
				go func() {
					res, err := server.Client().Do(req)
					if err == nil {
						_, err = io.Copy(io.Discard, res.Body)
						res.Body.Close()
					}
					done <- err
				}()
				select {
				case <-pause.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("current SQL read not reached")
				}
				cancel()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Fatal("canceled current read handler still running")
				}
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("disconnect=%v", err)
				}
				if generated.Load() != 0 {
					t.Fatal("disconnected read generated")
				}
				records, count, err := fx.audits.List(ctx, 0, 10)
				if err != nil || count != 1 {
					t.Fatalf("cancellation audit count=%d err=%v", count, err)
				}
				record, err := fx.audits.Get(ctx, records[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if record.AttemptCount != 0 || len(record.GenerationUsages) != 0 || record.CostInUSDTicks != 0 || record.AccountID != nil {
					t.Fatal("disconnected current read recorded attempt/account/cost")
				}
				key, err := fx.clients.Get(ctx, fx.created.Key.ID)
				if err != nil || key.ReservedUsageUSDTicks != 0 || key.BilledUsageUSDTicks != 0 {
					t.Fatal("disconnected current read retained billing")
				}
				for _, key := range []string{repository.AccountConcurrencyKey(fx.account.ID), fmt.Sprintf("client:%d", fx.created.Key.ID)} {
					if count, err := fx.concurrency.Current(ctx, key); err != nil || count != 0 {
						t.Fatalf("current read disconnect leaked capacity: %d %v", count, err)
					}
				}
			})
		}
	}
}
