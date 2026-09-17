package inference

import (
	"bytes"
	"context"
	"errors"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	physical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// The wrapper observes the worker context and delegates all generation to the
// real adapter. Cancellation, persisted claims and recovery use the public path.
type videoDeadlineObserver struct {
	provider.VideoAdapter
	mu        sync.Mutex
	deadlines []time.Time
}

func (a *videoDeadlineObserver) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	deadline, _ := ctx.Deadline()
	a.mu.Lock()
	a.deadlines = append(a.deadlines, deadline)
	a.mu.Unlock()
	return a.VideoAdapter.GenerateVideo(ctx, request)
}

func TestVideoPhysicalReceiptsAndFixedDeadline(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		for _, exhausted := range []bool{false, true} {
			name := string(kind) + "/remaining"
			if exhausted {
				name = string(kind) + "/exhausted_resume"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var calls, creates atomic.Int32
				var recovered atomic.Bool
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost {
						creates.Add(1)
						if kind != account.ProviderWeb {
							_, _ = w.Write([]byte(`{"request_id":"limited-native"}`))
							return
						}
					}
					if !recovered.Load() {
						once.Do(func() { close(entered) })
						select {
						case <-release:
						case <-ctx.Done():
						}
						return
					}
					_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
				})
				defer upstream.Close()
				model := "grok-imagine-video"
				if kind == account.ProviderBuild {
					model = "grok-imagine-video-1.5"
				}
				var observer *videoDeadlineObserver
				fx := newProviderCompletionFixture(t, upstream.URL, model, kind, nil, func(a provider.Adapter) provider.Adapter {
					observer = &videoDeadlineObserver{VideoAdapter: a.(provider.VideoAdapter)}
					return observer
				})
				if kind == account.ProviderBuild {
					credential := fx.account
					credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
					if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
						t.Fatal(err)
					}
				}
				fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
				job := createVideoAuthorizationJob(t, fx)
				if exhausted {
					limit := uint32(2)
					if kind == account.ProviderConsole {
						limit = 3
					}
					if kind == account.ProviderWeb {
						limit = 1
					}
					installVideoTestLimits(t, fx, job, limit, time.Now().Add(time.Hour), false)
				}
				firstCtx, firstCancel := context.WithCancel(context.Background())
				firstDone := make(chan struct{})
				go func() { fx.service.RunVideoWorkers(firstCtx); close(firstDone) }()
				defer func() { firstCancel(); <-firstDone }()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("initial network call missing")
				}
				firstCancel()
				select {
				case <-firstDone:
				case <-ctx.Done():
					t.Fatal("worker did not drain")
				}
				close(release)
				stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status != media.StatusInProgress {
					t.Fatalf("not recoverable: %+v", stored)
				}
				expired := time.Now().UTC().Add(-time.Second)
				stored.LeaseUntil = &expired
				if err := fx.jobs.UpdateMediaJob(ctx, stored); err != nil {
					t.Fatal(err)
				}
				recovered.Store(true)
				fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
				if err := fx.service.RecoverVideoJobs(ctx); err != nil {
					t.Fatal(err)
				}
				secondCtx, secondCancel := context.WithCancel(context.Background())
				secondDone := make(chan struct{})
				go func() { fx.service.RunVideoWorkers(secondCtx); close(secondDone) }()
				defer func() { secondCancel(); <-secondDone }()
				record := waitVoiceAudit(t, fx.audits)
				assertVideoBilling(t, fx, record)
				secondCancel()
				<-secondDone
				observer.mu.Lock()
				deadlines := append([]time.Time(nil), observer.deadlines...)
				observer.mu.Unlock()
				if kind != account.ProviderWeb && (len(deadlines) != 2 || deadlines[0].IsZero() || !deadlines[0].Equal(deadlines[1])) {
					t.Errorf("recovery reset the original deadline: %v", deadlines)
				}
				fx.receipts.mu.Lock()
				facts := append(fx.receipts.facts[:0:0], fx.receipts.facts...)
				fx.receipts.mu.Unlock()
				want := int(calls.Load())
				if kind == account.ProviderConsole {
					want++
				} // cold DPoP metadata request
				if len(facts) != want {
					t.Errorf("physical receipts=%d want actual submissions=%d", len(facts), want)
				}
				ids := map[string]bool{}
				for _, fact := range facts {
					if fact.Attempt.ID == "" || ids[fact.Attempt.ID] || fact.Attempt.AccountID != fx.account.ID {
						t.Errorf("missing, duplicate or foreign physical identity: %+v", fact.Attempt)
					}
					ids[fact.Attempt.ID] = true
				}
				if creates.Load() != 1 {
					t.Errorf("unexpected generation replay: %d", creates.Load())
				}
				stored, err = fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Limits.Reserved != uint32(want) || stored.Limits.Confirmed != uint32(want) || record.PhysicalReceipt != "committed" {
					t.Errorf("lost cross-recovery counters: limits=%+v receipt=%s want=%d", stored.Limits, record.PhysicalReceipt, want)
				}
				if len(deadlines) > 0 && !deadlines[0].Equal(*stored.Limits.Deadline) {
					t.Errorf("runtime deadline differs from persisted deadline")
				}

				if exhausted && kind != account.ProviderWeb && (stored.ErrorCode != "physical_budget_exhausted" || record.GenerationOutcome != "unconfirmed" || record.EstimatedCostInUSDTicks != 0) {
					t.Errorf("recovery enlarged exhausted budget: %+v", stored)
				}
			})
		}
	}
}

func installVideoTestLimits(t *testing.T, fx voiceCompletionFixture, job media.Job, limit uint32, deadline time.Time, consumed bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	claim, ok, err := fx.jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Minute), "test-policy-adoption")
	if err != nil || !ok {
		t.Fatalf("adopt claim=%v err=%v", ok, err)
	}
	deadline = deadline.UTC().Truncate(time.Microsecond)
	if err := fx.jobs.StartMediaJobExecutionLimits(ctx, job.ID, claim.ClaimToken, media.ExecutionLimits{Version: 1, Deadline: &deadline, PhysicalLimit: limit}); err != nil {
		t.Fatal(err)
	}
	if consumed {
		if err := fx.jobs.ReserveMediaJobPhysicalCall(ctx, job.ID, claim.ClaimToken, now); err != nil {
			t.Fatal(err)
		}
	}
	expired := now.Add(-time.Second)
	claim.LeaseUntil = &expired
	if err := fx.jobs.UpdateMediaJob(ctx, claim); err != nil {
		t.Fatal(err)
	}
}

type videoLimitsFaultRepository struct {
	repository.MediaJobRepository
	reserve, confirm bool
	reserveAckLost   bool
	confirmAckLost   atomic.Bool
}

func (r *videoLimitsFaultRepository) ReserveMediaJobPhysicalCall(ctx context.Context, id, claim string, now time.Time) error {
	if r.reserve {
		return errors.New("injected persistent permit failure")
	}
	err := r.MediaJobRepository.ReserveMediaJobPhysicalCall(ctx, id, claim, now)
	if err == nil && r.reserveAckLost {
		return errors.New("permit committed but acknowledgement lost")
	}
	return err
}
func (r *videoLimitsFaultRepository) ConfirmMediaJobPhysicalCalls(ctx context.Context, id, claim string, previous, confirmed uint32) error {
	if r.confirm {
		return errors.New("injected receipt counter failure")
	}
	err := r.MediaJobRepository.ConfirmMediaJobPhysicalCalls(ctx, id, claim, previous, confirmed)
	if err == nil && r.confirmAckLost.CompareAndSwap(true, false) {
		return errors.New("receipt counter committed but acknowledgement lost")
	}
	return err
}

func TestVideoExecutionLimitFailureBoundaries(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		for _, stage := range []string{"exhausted_before_first", "reserve_storage", "reserve_ack_lost", "receipt_storage", "confirm_storage", "confirm_ack_lost", "deadline_before_first", "deadline_inflight", "no_receipt_sink"} {
			t.Run(string(kind)+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var calls atomic.Int32
				release := make(chan struct{})
				upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					w.Header().Set("Content-Type", "application/json")
					if stage == "deadline_inflight" {
						select {
						case <-release:
						case <-ctx.Done():
						}
						return
					}
					if kind != account.ProviderWeb && r.Method == http.MethodPost {
						_, _ = w.Write([]byte(`{"request_id":"bounded-native"}`))
						return
					}
					if kind == account.ProviderWeb {
						_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
					} else {
						_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
					}
				})
				defer upstream.Close()
				defer close(release)
				model := "grok-imagine-video"
				if kind == account.ProviderBuild {
					model = "grok-imagine-video-1.5"
				}
				fx := newMediaCompletionFixture(t, upstream.URL, model, kind, nil)
				if kind == account.ProviderBuild {
					credential := fx.account
					credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
					if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
						t.Fatal(err)
					}
				}
				fx.receipts.fail = stage == "receipt_storage"
				if stage == "no_receipt_sink" {
					fx.service.SetQualityEventRecorder(nil)
				}
				jobs := &videoLimitsFaultRepository{MediaJobRepository: fx.jobs, reserve: stage == "reserve_storage", reserveAckLost: stage == "reserve_ack_lost", confirm: stage == "confirm_storage"}
				jobs.confirmAckLost.Store(stage == "confirm_ack_lost")
				var registry *qualityregistry.Registry
				if stage == "confirm_ack_lost" {
					registry = useVideoPhysicalJournal(t, fx)
				}
				fx.service.ConfigureMedia(jobs, mediaapp.NewVideoResources(jobs, nil), 1)
				fx.service.UpdateVideoMaxAttempts(3)
				job := createVideoAuthorizationJob(t, fx)
				if stage == "exhausted_before_first" {
					installVideoTestLimits(t, fx, job, 1, time.Now().Add(time.Hour), true)
				}
				if stage == "deadline_before_first" {
					installVideoTestLimits(t, fx, job, 10, time.Now().Add(-time.Second), false)
				}
				if stage == "deadline_inflight" {
					installVideoTestLimits(t, fx, job, 10, time.Now().Add(300*time.Millisecond), false)
				}
				workerCtx, stop := context.WithCancel(context.Background())
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
				wantCalls := int32(0)
				wantGeneration := "not_started"
				wantCode := "execution_unavailable"
				switch stage {
				case "exhausted_before_first":
					wantCode = "physical_budget_exhausted"
				case "deadline_before_first":
					wantCode = "execution_deadline"
				case "no_receipt_sink":
					wantCalls = 2
					if kind == account.ProviderWeb {
						wantCalls = 1
					}
					wantGeneration = "completed"
					wantCode = "generation_failed"
				case "deadline_inflight":
					wantCode = "execution_deadline"
					wantCalls = 1
					wantGeneration = "unconfirmed"
				case "receipt_storage", "confirm_storage", "confirm_ack_lost":
					if kind == account.ProviderBuild {
						wantCalls = 1
						wantGeneration = "unconfirmed"
					}
					if kind == account.ProviderWeb {
						wantCalls = 1
						wantGeneration = "completed"
						wantCode = "generation_failed"
					}
				}
				if calls.Load() != wantCalls || record.GenerationOutcome != wantGeneration || stored.ErrorCode != wantCode {
					t.Fatalf("actual=%d want=%d generation=%s want=%s code=%s want=%s limits=%+v", calls.Load(), wantCalls, record.GenerationOutcome, wantGeneration, stored.ErrorCode, wantCode, stored.Limits)
				}
				if wantGeneration == "completed" && record.EstimatedCostInUSDTicks <= 0 {
					t.Fatal("local receipt failure erased known generation cost")
				}
				if wantGeneration != "completed" && record.EstimatedCostInUSDTicks != 0 {
					t.Fatal("local stop invented generation cost")
				}
				original, err := fx.accounts.Get(ctx, fx.account.ID)
				if err != nil {
					t.Fatal(err)
				}
				if original.FailureCount != 0 || original.CooldownUntil != nil || original.RefreshFailureCount != 0 {
					t.Fatalf("local execution gate damaged account health: %+v", original)
				}
				if stage == "receipt_storage" || stage == "confirm_storage" {
					if record.PhysicalReceipt != "failed" || stored.Limits.Confirmed != 0 {
						t.Fatalf("failed receipt confirmed: %s %+v", record.PhysicalReceipt, stored.Limits)
					}
				}
				if stage == "no_receipt_sink" && (stored.Limits.Confirmed != 0 || record.PhysicalReceipt != "not_required") {
					t.Fatalf("absent receipt store forged persistence: %+v %s", stored.Limits, record.PhysicalReceipt)
				}
				if stage == "reserve_ack_lost" && (stored.Limits.Reserved != 1 || stored.Limits.Confirmed != 0 || record.PhysicalReceipt != "unconfirmed") {
					t.Fatalf("ambiguous permit falsely confirmed: %+v receipt=%s", stored.Limits, record.PhysicalReceipt)
				}
				if stage == "confirm_ack_lost" {
					var rows []journal.EventRow
					if err := registry.DB().Where("stage = ?", "exchange").Find(&rows).Error; err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 || stored.Limits.Reserved != 1 || stored.Limits.Confirmed != 1 {
						t.Fatalf("lost receipt acknowledgement duplicated a fact: rows=%d limits=%+v", len(rows), stored.Limits)
					}
					wantReceipt := "committed"
					if kind == account.ProviderWeb {
						wantReceipt = "failed"
					}
					if record.PhysicalReceipt != wantReceipt {
						t.Fatalf("receipt=%s want=%s", record.PhysicalReceipt, wantReceipt)
					}
				}
				if stage == "exhausted_before_first" && record.PhysicalReceipt != "unconfirmed" {
					t.Fatalf("uncertain old permit reported as confirmed: %s", record.PhysicalReceipt)
				}
			})
		}
	}
}

type videoPollingObserver struct {
	provider.VideoAdapter
	peak     atomic.Int32
	observed atomic.Int32
}

func (a *videoPollingObserver) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	progress := request.Progress
	request.Progress = func(value int) {
		a.observed.Add(1)
		retained := int32(len(physical.PhysicalObservations(ctx)))
		for current := a.peak.Load(); retained > current; current = a.peak.Load() {
			if a.peak.CompareAndSwap(current, retained) {
				break
			}
		}
		if progress != nil {
			progress(value)
		}
	}
	return a.VideoAdapter.GenerateVideo(ctx, request)
}

// Uses the production two-second polling cadence. The elapsed run exercises
// real HTTP bodies, SQL claim budget and acknowledgement pruning past 128 calls.
func TestVideoLongPollingUsesBoundedPhysicalReceipts(t *testing.T) {
	if testing.Short() {
		t.Skip("real 130-poll integration takes about 258 seconds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	var calls, polls atomic.Int32
	terminal := make(chan struct{})
	upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"request_id":"long-native"}`))
			return
		}
		if polls.Add(1) < 130 {
			_, _ = w.Write([]byte(`{"status":"processing","progress":50}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
		close(terminal)
	})
	defer upstream.Close()
	var observer *videoPollingObserver
	fx := newProviderCompletionFixture(t, upstream.URL, "grok-imagine-video-1.5", account.ProviderBuild, nil, func(a provider.Adapter) provider.Adapter {
		observer = &videoPollingObserver{VideoAdapter: a.(provider.VideoAdapter)}
		return observer
	})
	registry := useVideoPhysicalJournal(t, fx)
	credential := fx.account
	credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
	if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
		t.Fatal(err)
	}
	fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
	job := createVideoAuthorizationJob(t, fx)
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { fx.service.RunVideoWorkers(workerCtx); close(done) }()
	defer func() { stop(); <-done }()
	select {
	case <-terminal:
	case <-ctx.Done():
		t.Fatalf("long poll stopped early: calls=%d polls=%d", calls.Load(), polls.Load())
	}
	record := waitVoiceAudit(t, fx.audits)
	stop()
	<-done
	assertVideoBilling(t, fx, record)
	stored, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
	if err != nil {
		t.Fatal(err)
	}
	fx.receipts.mu.Lock()
	facts := append(fx.receipts.facts[:0:0], fx.receipts.facts...)
	fx.receipts.mu.Unlock()
	if calls.Load() != 131 || len(facts) != 131 || stored.Limits.Reserved != 131 || stored.Limits.Confirmed != 131 || record.PhysicalReceipt != "committed" || record.GenerationOutcome != "completed" {
		t.Fatalf("calls=%d receipts=%d limits=%+v physical=%s generation=%s", calls.Load(), len(facts), stored.Limits, record.PhysicalReceipt, record.GenerationOutcome)
	}
	if observer.observed.Load() != 131 || observer.peak.Load() > 2 {
		t.Fatalf("unbounded retained facts: observations=%d peak=%d", observer.observed.Load(), observer.peak.Load())
	}
	var rows []journal.EventRow
	if err := registry.DB().Where("stage = ?", "exchange").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 131 {
		t.Fatalf("actual durable journal rows=%d", len(rows))
	}
	t.Logf("actual submissions=%d persisted receipts=%d peak retained entries=%d", calls.Load(), len(facts), observer.peak.Load())
}

func TestVideoCredentialsAndDownloadsSharePhysicalBudget(t *testing.T) {
	if !imageAssetTLSChild(t) {
		return
	}
	for _, tc := range []struct {
		kind  account.Provider
		stage string
	}{
		{account.ProviderBuild, "download_allowed"}, {account.ProviderBuild, "download_exhausted"},
		{account.ProviderConsole, "download_allowed"}, {account.ProviderConsole, "download_exhausted"},
		{account.ProviderWeb, "download_allowed"}, {account.ProviderWeb, "download_exhausted"},
		{account.ProviderBuild, "oauth_allowed"}, {account.ProviderBuild, "oauth_exhausted"},
		{account.ProviderBuild, "oauth_store_failure"}, {account.ProviderBuild, "oauth_deadline"},
	} {
		t.Run(string(tc.kind)+"/"+tc.stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var calls, downloads, oauth atomic.Int32
			release := make(chan struct{})
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.kind != account.ProviderWeb && r.Method == http.MethodPost {
					_, _ = w.Write([]byte(`{"request_id":"asset-native"}`))
					return
				}
				if tc.kind == account.ProviderConsole {
					_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
					return
				}
				if tc.kind == account.ProviderWeb {
					_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
				} else {
					_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://assets.grok.com/video.mp4"}}`))
				}
			})
			defer upstream.Close()
			defer close(release)
			model := "grok-imagine-video"
			if tc.kind == account.ProviderBuild {
				model = "grok-imagine-video-1.5"
			}
			fx := newMediaCompletionFixture(t, upstream.URL, model, tc.kind, func(store provider.ImageAssetStore) provider.ImageAssetStore { return store })
			usesOAuth := strings.HasPrefix(tc.stage, "oauth_")
			if tc.kind == account.ProviderBuild {
				credential := fx.account
				credential.BuildSuperEntitled, credential.BuildRouteMode = true, account.BuildRouteBuild
				if usesOAuth {
					credential.AuthType = account.AuthTypeOAuth
					credential.EncryptedRefreshToken = credential.EncryptedAccessToken
					credential.ExpiresAt = time.Now().Add(time.Minute)
				}
				if _, err := fx.accounts.UpdateAdministration(ctx, credential.ID, repository.AccountAdminPatch{BuildSuperEntitled: &credential.BuildSuperEntitled, BuildRouteMode: &credential.BuildRouteMode}); err != nil {
					t.Fatal(err)
				}
				if usesOAuth {
					if _, _, err := fx.accounts.UpsertByIdentity(ctx, credential); err != nil {
						t.Fatal(err)
					}
				}
			}
			payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 64)...)
			fx.useProxy(imageAssetProxy(t, upstream.URL, func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "auth.x.ai" {
					oauth.Add(1)
					if r.URL.Path != "/oauth2/token" {
						t.Errorf("unexpected OAuth endpoint %s", r.URL.Path)
					}
					if tc.stage == "oauth_deadline" {
						select {
						case <-release:
						case <-ctx.Done():
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"access_token":"synthetic-rotated","refresh_token":"synthetic-refresh","expires_in":3600}`))
					return
				}
				downloads.Add(1)
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write(payload)
			}))
			jobs := &videoLimitsFaultRepository{MediaJobRepository: fx.jobs, reserve: tc.stage == "oauth_store_failure"}
			fx.service.ConfigureMedia(jobs, mediaapp.NewVideoResources(jobs, nil), 1)
			fx.service.ConfigureMediaAssets(fx.media)
			job := createVideoAuthorizationJob(t, fx)
			if tc.stage == "download_exhausted" {
				limit := uint32(2)
				if tc.kind == account.ProviderConsole {
					limit = 3
				}
				if tc.kind == account.ProviderWeb {
					limit = 1
				}
				installVideoTestLimits(t, fx, job, limit, time.Now().Add(time.Hour), false)
			}
			if tc.stage == "oauth_exhausted" {
				installVideoTestLimits(t, fx, job, 1, time.Now().Add(time.Hour), true)
			}
			if tc.stage == "oauth_deadline" {
				installVideoTestLimits(t, fx, job, 10, time.Now().Add(300*time.Millisecond), false)
			}
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
			wantedDownloads := int32(0)
			wantedOAuth := int32(0)
			wantedGeneration := "completed"
			if tc.stage == "download_allowed" || tc.stage == "oauth_allowed" {
				wantedDownloads = 1
				if stored.Status != media.StatusCompleted {
					t.Fatalf("archive failed: %s", stored.ErrorMessage)
				}
			}
			if tc.stage == "oauth_allowed" || tc.stage == "oauth_deadline" {
				wantedOAuth = 1
			}
			if usesOAuth && tc.stage != "oauth_allowed" {
				wantedGeneration = "not_started"
			}
			if downloads.Load() != wantedDownloads || oauth.Load() != wantedOAuth || record.GenerationOutcome != wantedGeneration {
				t.Fatalf("downloads=%d want=%d OAuth=%d want=%d generation=%s want=%s job=%+v", downloads.Load(), wantedDownloads, oauth.Load(), wantedOAuth, record.GenerationOutcome, wantedGeneration, stored)
			}
			if wantedGeneration == "completed" && record.EstimatedCostInUSDTicks <= 0 {
				t.Fatal("known generated cost missing")
			}
			if wantedDownloads == 1 {
				_, body, err := fx.media.OpenVideo(ctx, stored.ResultAssetID)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(body)
				_ = body.Close()
				if err != nil || !bytes.Equal(data, payload) {
					t.Fatal("archived bytes differ")
				}
			}
			fx.receipts.mu.Lock()
			facts := append(fx.receipts.facts[:0:0], fx.receipts.facts...)
			fx.receipts.mu.Unlock()
			expected := calls.Load() + downloads.Load() + oauth.Load()
			if tc.kind == account.ProviderConsole {
				expected++
			}
			if len(facts) != int(expected) {
				t.Fatalf("physical receipts=%d actual=%d", len(facts), expected)
			}
			var authFacts, assetFacts int
			for _, fact := range facts {
				if fact.Attempt.AccountID != fx.account.ID {
					t.Fatal("preparation lost the selected account")
				}
				if fact.Stage == "credential_prepare" {
					authFacts++
				}
				if fact.Stage == "asset_download" {
					assetFacts++
				}
			}
			if usesOAuth && authFacts != int(oauth.Load()) {
				t.Fatalf("OAuth preparation receipt mismatch %d", authFacts)
			}
			if assetFacts != int(downloads.Load()) {
				t.Fatalf("asset receipt mismatch %d", assetFacts)
			}
			if tc.stage == "download_exhausted" && stored.ErrorCode != "physical_budget_exhausted" {
				t.Fatalf("download budget ignored: %s", stored.ErrorCode)
			}
			original, err := fx.accounts.Get(ctx, fx.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if usesOAuth && tc.stage != "oauth_allowed" && (original.RefreshFailureCount != 0 || original.RefreshPermanent || original.FailureCount != 0) {
				t.Fatalf("local gate damaged OAuth state: %+v", original)
			}
		})
	}
}

type videoJournalReceiptSink struct {
	*voiceReceiptSink
	store *journal.Store
}

func (s *videoJournalReceiptSink) RecordPhysicalEvents(ctx context.Context, facts []attemptmeta.PhysicalFact) error {
	events := make([]qualitymodel.Event, 0, len(facts))
	for _, fact := range facts {
		events = append(events, qualitymodel.Event{Attempt: fact.Attempt, Stage: "exchange", Outcome: "observed", At: fact.At, Physical: &fact})
	}
	if err := s.store.RecordMany(ctx, events); err != nil {
		return err
	}
	return s.voiceReceiptSink.RecordPhysicalEvents(ctx, facts)
}
func useVideoPhysicalJournal(t *testing.T, fx voiceCompletionFixture) *qualityregistry.Registry {
	t.Helper()
	r, err := qualityregistry.Open(context.Background(), qualityregistry.Options{Driver: "sqlite", SQLitePath: fx.dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	fx.service.SetQualityEventRecorder(&videoJournalReceiptSink{voiceReceiptSink: fx.receipts, store: journal.New(r.DB())})
	return r
}
