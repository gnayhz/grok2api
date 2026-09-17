package inference

import (
	"context"
	"errors"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type videoQuotaFaultAccounts struct {
	repository.AccountRepository
	active  atomic.Bool
	ackLost bool
}

func (r *videoQuotaFaultAccounts) ConsumeQuota(ctx context.Context, value account.QuotaConsumption, now time.Time) (account.QuotaConsumptionReceipt, error) {
	if r.active.Load() && !r.ackLost {
		return account.QuotaConsumptionReceipt{}, errors.New("injected quota storage unavailable")
	}
	receipt, err := r.AccountRepository.ConsumeQuota(ctx, value, now)
	if err == nil && r.active.Load() {
		return receipt, errors.New("injected committed quota acknowledgement lost")
	}
	return receipt, err
}

type videoQuotaFaultJobs struct {
	repository.MediaJobRepository
	active  atomic.Bool
	ackLost bool
}

func (r *videoQuotaFaultJobs) MarkMediaJobQuotaRecorded(ctx context.Context, value media.Job, now time.Time) error {
	if r.active.Load() && !r.ackLost {
		return errors.New("injected job quota marker unavailable")
	}
	err := r.MediaJobRepository.MarkMediaJobQuotaRecorded(ctx, value, now)
	if err == nil && r.active.Load() {
		return errors.New("injected job quota marker acknowledgement lost")
	}
	return err
}

func TestVideoQuotaHandoffRecoversWithoutRegeneration(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderConsole, account.ProviderWeb} {
		for _, fault := range []string{"accept_failed", "accept_ack_lost", "marker_failed", "marker_ack_lost", "new_snapshot", "account_deleted"} {
			t.Run(fmt.Sprintf("%s/%s", kind, fault), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var calls, generated atomic.Int32
				upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					w.Header().Set("Content-Type", "application/json")
					if strings.HasSuffix(r.URL.Path, "/usage") {
						_, _ = w.Write([]byte(`{"quotas":[{"kind":"chat","limit":20,"remaining":20},{"kind":"image","limit":20,"remaining":20},{"kind":"video","limit":20,"remaining":17,"used":3}]}`))
						return
					}
					if strings.HasSuffix(r.URL.Path, "/quota_info") {
						_, _ = w.Write([]byte(`{"image":null,"imagePro":null,"imageEdit":null,"video":null,"video720p":{"available":true,"remainingQueries":17,"windowSizeSeconds":86400}}`))
						return
					}
					if r.Method == http.MethodPost {
						generated.Add(1)
					}
					if kind == account.ProviderWeb {
						_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
					} else if r.Method == http.MethodPost {
						_, _ = w.Write([]byte(`{"request_id":"quota-handoff"}`))
					} else {
						_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
					}
				})
				defer upstream.Close()
				var accountFault *videoQuotaFaultAccounts
				fx := newProviderCompletionFixtureWithAccounts(t, upstream.URL, "grok-imagine-video", kind, nil, nil, func(repo repository.AccountRepository) repository.AccountRepository {
					accountFault = &videoQuotaFaultAccounts{AccountRepository: repo, ackLost: fault == "accept_ack_lost"}
					accountFault.active.Store(fault == "accept_failed" || fault == "accept_ack_lost" || fault == "new_snapshot" || fault == "account_deleted")
					return accountFault
				})
				mode := console.QuotaModeVideo
				if kind == account.ProviderWeb {
					mode = account.QuotaModeWebVideo720p
				}
				if err := saveQuotaWindowsFixture(fx.accounts, ctx, fx.account.ID, account.WebTierBasic, time.Now().UTC(), []account.QuotaWindow{{Mode: mode, Remaining: 20, Total: 20}}); err != nil {
					t.Fatal(err)
				}
				jobFault := &videoQuotaFaultJobs{MediaJobRepository: fx.jobs, ackLost: fault == "marker_ack_lost"}
				jobFault.active.Store(fault == "marker_failed" || fault == "marker_ack_lost")
				fx.service.ConfigureMedia(jobFault, mediaapp.NewVideoResources(jobFault, nil), 1)
				job := createVideoAuthorizationJob(t, fx)
				workerCtx, stop := context.WithCancel(ctx)
				done := make(chan struct{})
				go func() { fx.service.RunVideoWorkers(workerCtx); close(done) }()
				defer func() { stop(); <-done }()
				record := waitVoiceAudit(t, fx.audits)
				stop()
				<-done
				assertVideoBilling(t, fx, record)
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Execution.Phase != media.VideoExecutionGenerated || stored.Status != media.StatusFailed || stored.Quota.Mode != mode || stored.Quota.AccountID != fx.account.ID || stored.Quota.SnapshotVersion == 0 {
					t.Fatalf("missing durable source: %+v", stored)
				}
				if fault != "marker_ack_lost" && stored.Quota.RecordedAt != nil {
					t.Fatal("failed handoff claimed an acknowledgement")
				}
				if stored.Quota.RecordedAt == nil {
					if err := fx.jobs.DeleteMediaJob(ctx, job.ID); !errors.Is(err, repository.ErrConflict) {
						t.Fatalf("pending source could be deleted: %v", err)
					}
					if _, err := fx.clients.DeleteMany(ctx, []uint64{job.ClientKeyID}); !errors.Is(err, repository.ErrConflict) {
						t.Fatalf("key cleanup deleted pending source: %v", err)
					}
				}
				want := 19
				if fault == "new_snapshot" {
					want = 17
					if err := saveQuotaWindowsFixture(fx.accounts, ctx, fx.account.ID, account.WebTierBasic, time.Now().UTC(), []account.QuotaWindow{{Mode: mode, Remaining: want, Total: 20}}); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "account_deleted" {
					if err := fx.accounts.Delete(ctx, fx.account.ID); err != nil {
						t.Fatal(err)
					}
				}
				// Reconstruct account, gateway and selector owners on another SQL
				// connection. No in-memory deduplication/worker state survives.
				db, err := relational.OpenSQLite(ctx, fx.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				accounts, jobs := relational.NewAccountRepository(db), relational.NewMediaJobRepository(db)
				concurrency, sticky := memory.NewConcurrencyLimiter(), memory.NewStickyStore()
				accountService := accountapp.NewService(accounts, fx.audits, memory.NewDeviceSessionStore(), sticky, fx.registry, nil, security.RandomTokenSource{}, nil, nil, nil)
				clients := clientkeyapp.NewService("test-owner", fx.clients, memory.NewRateLimiter(), concurrency, 120, 4, nil, security.RandomTokenSource{})
				defer closeClientKeyService(t, clients)
				selector := selector.NewSelector(accounts, concurrency, sticky, fx.registry, time.Hour, time.Second, time.Minute)
				restarted := gateway.NewService(fx.models, fx.audits, accountService, clients, fx.registry, selector, historyapp.NewResponseResources(relational.NewResponseRepository(db)), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 1)
				restarted.ConfigureMedia(jobs, mediaapp.NewVideoResources(jobs, nil), 1)
				for range 3 {
					if err := restarted.RecoverVideoJobs(ctx); err != nil {
						t.Fatal(err)
					}
				}
				stored, err = jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
				if err != nil || stored.Quota.RecordedAt == nil {
					t.Fatalf("handoff not recovered: %+v %v", stored, err)
				}
				fact := account.QuotaConsumption{EventID: "video_quota_" + job.ID, AccountID: fx.account.ID, Mode: mode, SnapshotVersion: stored.Quota.SnapshotVersion, Units: 1}
				receipt, err := accounts.ConsumeQuota(ctx, fact, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				wantState := account.QuotaConsumptionApplied
				if fault == "new_snapshot" {
					wantState = account.QuotaConsumptionPendingRefresh
				}
				if fault == "account_deleted" {
					wantState = account.QuotaConsumptionAccountDeleted
				}
				if receipt.State != wantState {
					t.Fatalf("receipt=%+v expected=%s", receipt, wantState)
				}
				if fault == "new_snapshot" {
					// A further account owner starts with an empty queue and no shared
					// Redis demand; recovery must discover the SQL pending receipt.
					fresh := accountapp.NewService(accounts, fx.audits, memory.NewDeviceSessionStore(), memory.NewStickyStore(), fx.registry, nil, security.RandomTokenSource{}, nil, nil, nil)
					refreshCtx, stopRefresh := context.WithCancel(ctx)
					refreshDone := make(chan struct{})
					go func() { fresh.RunQuotaRefresh(refreshCtx); close(refreshDone) }()
					defer func() { stopRefresh(); <-refreshDone }()
					for {
						pending, err := accounts.ListPendingQuotaRefreshes(ctx, 0, 100)
						if err != nil {
							t.Fatal(err)
						}
						if len(pending) == 0 {
							break
						}
						select {
						case <-ctx.Done():
							t.Fatal("persisted quota demand was not recovered")
						case <-time.After(10 * time.Millisecond):
						}
					}
					stopRefresh()
					<-refreshDone
					receipt, err = accounts.ConsumeQuota(ctx, fact, time.Now().UTC())
					if err != nil || receipt.State != account.QuotaConsumptionRefreshed {
						t.Fatalf("recovered remote receipt=%+v %v", receipt, err)
					}
				}
				windows, err := accounts.GetQuotaWindows(ctx, []uint64{fx.account.ID})
				if err != nil {
					t.Fatal(err)
				}
				if fault == "account_deleted" {
					if len(windows[fx.account.ID]) != 0 {
						t.Fatal("deleted account quota recreated")
					}
				} else {
					found := false
					for _, window := range windows[fx.account.ID] {
						if window.Mode == mode {
							found = true
							if window.Remaining != want {
								t.Fatalf("replay quota=%+v want=%d", windows, want)
							}
						}
					}
					if !found {
						t.Fatal("expected quota mode disappeared")
					}
				}
				assertVideoBilling(t, fx, record)
				if generated.Load() != 1 {
					t.Fatalf("recovery regenerated: %d", generated.Load())
				}
			})
		}
	}
}

func TestVideoGeneratedArchiveFailureConsumesQuota(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderConsole, account.ProviderWeb} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var calls atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				if kind == account.ProviderConsole && r.Method == http.MethodPost {
					_, _ = w.Write([]byte(`{"request_id":"quota-native"}`))
					return
				}
				if kind == account.ProviderWeb {
					_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
				} else {
					_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
				}
			})
			defer upstream.Close()
			fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-video", kind, nil)
			mode := console.QuotaModeVideo
			if kind == account.ProviderWeb {
				mode = account.QuotaModeWebVideo720p
			}
			if err := saveQuotaWindowsFixture(fx.accounts, ctx, fx.account.ID, account.WebTierBasic, time.Now().UTC(), []account.QuotaWindow{{Mode: mode, Remaining: 20, Total: 20}}); err != nil {
				t.Fatal(err)
			}
			fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
			job := createVideoAuthorizationJob(t, fx)
			workerCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(workerCtx); close(done) }()
			defer func() { stop(); <-done }()
			record := waitVoiceAudit(t, fx.audits)
			stop()
			<-done
			assertVideoBilling(t, fx, record)
			stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != media.StatusFailed || stored.Execution.Phase != media.VideoExecutionGenerated || record.GenerationOutcome != "completed" || record.EstimatedCostInUSDTicks <= 0 {
				t.Fatalf("fixture missed confirmed generation/archive failure: job=%+v record=%+v", stored, record)
			}
			windows, err := fx.accounts.GetQuotaWindows(ctx, []uint64{fx.account.ID})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, window := range windows[fx.account.ID] {
				if window.Mode == mode {
					found = true
					if window.Remaining != 19 {
						t.Errorf("confirmed generated video did not consume quota after archive failure: remaining=%d want=19", window.Remaining)
					}
				}
			}
			if !found {
				t.Fatal("quota window disappeared")
			}
		})
	}
}
