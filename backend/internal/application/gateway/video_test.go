package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestVideoQuotaModeUsesWeb720pProduct(t *testing.T) {
	tests := []struct {
		provider   account.Provider
		resolution string
		want       string
	}{
		{account.ProviderWeb, "", account.QuotaModeWebVideo720p},
		{account.ProviderWeb, "720p", account.QuotaModeWebVideo720p},
		{account.ProviderWeb, "480p", account.QuotaModeWebVideo},
		{account.ProviderConsole, "720p", account.QuotaModeWebVideo},
	}
	for _, test := range tests {
		if got := videoQuotaMode(test.provider, account.QuotaModeWebVideo, test.resolution); got != test.want {
			t.Fatalf("videoQuotaMode(%s, %q) = %q, want %q", test.provider, test.resolution, got, test.want)
		}
	}
}

func TestVideoQuotaFinalizationKeepsEffectiveConsumptionFence(t *testing.T) {
	refreshMode, decrementMode, availabilityMode := quotaFinalizationModes(account.QuotaModeWebVideo720p, account.QuotaGroupWebImagine)
	if refreshMode != account.QuotaGroupWebImagine || decrementMode != account.QuotaModeWebVideo720p || availabilityMode != "" {
		t.Fatalf("refresh=%q decrement=%q availability=%q", refreshMode, decrementMode, availabilityMode)
	}
	refreshMode, decrementMode, availabilityMode = quotaFinalizationModes("weekly", account.QuotaGroupWebImagine)
	if refreshMode != "weekly" || decrementMode != "weekly" || availabilityMode != account.QuotaGroupWebImagine {
		t.Fatalf("shared weekly refresh=%q decrement=%q availability=%q", refreshMode, decrementMode, availabilityMode)
	}
}

func TestGetVideoExposesOnlyReadableResultAsset(t *testing.T) {
	completed := media.Job{
		ID: "video_status", ClientKeyID: 7, Status: media.StatusCompleted,
		ResultAssetID: "vid_local", UpstreamURL: "https://assets.grok.com/video.mp4",
	}
	tests := []struct {
		name  string
		store videoAssetStore
		want  string
	}{
		{
			name: "available",
			store: &videoAssetStoreStub{
				openAsset: media.Asset{ID: "vid_local", Kind: "video", MIMEType: "video/mp4"},
				openData:  []byte("video"),
			},
			want: "vid_local",
		},
		{name: "missing", store: &videoAssetStoreStub{openErr: media.ErrAssetNotFound}},
		{name: "archive unconfigured"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), mediaJobs: &videoUsageRepository{job: completed}, mediaAssets: test.store}
			service.ConfigureMedia(service.mediaJobs, mediaapp.NewVideoResources(&videoUsageRepository{job: completed}, nil), 1)
			job, err := service.GetVideo(context.Background(), completed.ID, clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: completed.ClientKeyID})
			if err != nil {
				t.Fatal(err)
			}
			if job.ResultAssetID != test.want {
				t.Fatalf("result asset ID = %q, want %q", job.ResultAssetID, test.want)
			}
		})
	}
}

func TestOpenVideoContentKeepsLocalAssetFastPath(t *testing.T) {
	completed := media.Job{
		ID: "video_content", ClientKeyID: 7, Status: media.StatusCompleted,
		ResultAssetID: "vid_local", UpstreamURL: "https://assets.grok.com/video.mp4",
	}
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(),
		mediaJobs: &videoUsageRepository{job: completed},
		mediaAssets: &videoAssetStoreStub{
			openAsset: media.Asset{ID: "vid_local", Kind: "video", MIMEType: "video/mp4", SizeBytes: 5},
			openData:  []byte("video"),
		},
	}
	service.ConfigureMedia(service.mediaJobs, mediaapp.NewVideoResources(&videoUsageRepository{job: completed}, nil), 1)
	body, contentType, size, err := service.OpenVideoContent(context.Background(), completed.ID, clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: completed.ClientKeyID})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "video" || contentType != "video/mp4" || size != 5 {
		t.Fatalf("content = %q, type = %q, size = %d", data, contentType, size)
	}
}

func TestRecoverVideoJobsRetriesUsageWithoutRegeneratingVideo(t *testing.T) {
	completedAt := time.Now().UTC()
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_usage_recovery", RequestID: "request-usage-recovery",
		ClientKeyID: 1, ClientKeyName: "client", AccountID: 2, AccountName: "account",
		Provider: "grok_web", Model: "custom-video", ModelRouteID: 3, UpstreamModel: "Web/grok-imagine-video",
		Seconds: 8, Quality: "720p", Status: media.StatusCompleted, InputImageCount: 2, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}}
	recorder := &durableVideoAuditRecorder{failures: 1}
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), mediaJobs: repository, audits: recorder}
	if err := service.RecoverVideoJobs(context.Background()); err == nil {
		t.Fatal("first durable audit failure was ignored")
	}
	if repository.job.UsageRecordedAt != nil {
		t.Fatal("usage was marked before durable audit commit")
	}
	if err := service.RecoverVideoJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.job.UsageRecordedAt == nil || recorder.calls != 2 {
		t.Fatalf("recordedAt = %v, audit calls = %d", repository.job.UsageRecordedAt, recorder.calls)
	}
	if recorder.last.EventID != "video_usage_video_usage_recovery" || recorder.last.EstimatedCostInUSDTicks <= 0 || recorder.last.MediaInputImages != 2 {
		t.Fatalf("audit = %#v", recorder.last)
	}
}

func TestVideoEditRouteUsesResolvedUpstreamInsteadOfPublicName(t *testing.T) {
	routes := []model.Route{
		{ID: 1, PublicID: "grok-imagine-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video"},
		{ID: 2, PublicID: "company-video-editor", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video"},
		{ID: 3, PublicID: "company-video-editor", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video-1.5"},
	}
	compatible, err := routesForVideoOperation(routes, provider.VideoOperationEdit)
	if err != nil {
		t.Fatal(err)
	}
	if len(compatible) != 1 || compatible[0].ID != 2 {
		t.Fatalf("compatible routes = %#v", compatible)
	}
	if _, err := routesForVideoOperation(routes[:1], provider.VideoOperationExtend); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("unsupported route error = %v", err)
	}
}

func TestVideoPricingLeavesUnmeasurableOperationsUnpriced(t *testing.T) {
	pricing, priced := resolveVideoPricing(provider.VideoOperationGenerate, "Console/grok-imagine-video", "720p", 6, 1)
	if !priced || pricing.CostInUSDTicks <= 0 {
		t.Fatalf("priced generation = %#v, priced=%v", pricing, priced)
	}
	if pricing, priced := resolveVideoPricing(provider.VideoOperationGenerate, "Console/grok-imagine-video", "", 6, 0); priced || pricing.CostInUSDTicks != 0 {
		t.Fatalf("generation without resolution = %#v, priced=%v", pricing, priced)
	}
	if pricing, priced := resolveVideoPricing(provider.VideoOperationExtend, "Console/grok-imagine-video", "", 6, 0); priced || pricing.CostInUSDTicks != 0 {
		t.Fatalf("extension without measurable input duration = %#v, priced=%v", pricing, priced)
	}
}

// 上游对 Console 视频的这两条限制原先只在 provider 层拦截，而生成接口是异步的，
// 客户端会先拿到 request_id 再从轮询里读到失败任务。入队前校验让错误立刻可见。
func TestVideoRouteParametersRejectConsoleReferenceLimits(t *testing.T) {
	// 实测：8 张 reference_images 上游回 400 "Too many reference images: 8. Maximum allowed is 7."
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video-1.5", "720p", false, 8, 6); !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("8 references error = %v", err)
	}
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video-1.5", "720p", false, 7, 6); err != nil {
		t.Fatalf("7 references error = %v", err)
	}
	// 实测：grok-imagine-video 的 reference-to-video 回 400
	// "Duration 15s exceeds the maximum allowed for reference-to-video, which is 10s."
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video", "720p", false, 1, 15); !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("base model reference duration error = %v", err)
	}
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video", "720p", false, 1, 10); err != nil {
		t.Fatalf("base model 10s reference error = %v", err)
	}
	// image-to-video（无 reference_images）与 1.5 都保持 15s。
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video", "720p", true, 0, 15); err != nil {
		t.Fatalf("base model text/first-frame 15s error = %v", err)
	}
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video-1.5", "720p", false, 2, 15); err != nil {
		t.Fatalf("1.5 multi-reference 15s error = %v", err)
	}
	// Build 使用相同的 1.5 上游模型名，但支持通用上限 8；不能因同名套用 Console 的 7 张限制。
	if err := validateVideoRouteParameters(account.ProviderBuild, provider.VideoOperationGenerate, "grok-imagine-video-1.5", "720p", false, 8, 15); err != nil {
		t.Fatalf("Build 1.5 references error = %v", err)
	}
	// Web 新协议只有文本生视频抓包证据；不能退回已删除的 media-post 旧链路。
	if err := validateVideoRouteParameters(account.ProviderWeb, provider.VideoOperationGenerate, "grok-imagine-video", "720p", false, 0, 15); err != nil {
		t.Fatalf("Web text video error = %v", err)
	}
	if err := validateVideoRouteParameters(account.ProviderWeb, provider.VideoOperationGenerate, "grok-imagine-video", "720p", true, 0, 6); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("Web image video error = %v", err)
	}
	if err := validateVideoRouteParameters(account.ProviderWeb, provider.VideoOperationGenerate, "grok-imagine-video", "720p", false, 1, 6); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("Web reference video error = %v", err)
	}
}

func TestRoutesForVideoParametersKeepsCompatibleSameNameProviders(t *testing.T) {
	routes := []model.Route{
		{ID: 1, PublicID: "shared-video", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video-1.5"},
		{ID: 2, PublicID: "shared-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video-1.5"},
	}
	compatible, err := routesForVideoParameters(routes, provider.VideoOperationGenerate, "720p", false, 8, 15)
	if err != nil {
		t.Fatal(err)
	}
	if len(compatible) != 1 || compatible[0].ID != 2 {
		t.Fatalf("compatible routes = %#v", compatible)
	}
	if _, err := routesForVideoParameters(routes[:1], provider.VideoOperationGenerate, "720p", false, 8, 15); !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("Console-only invalid route error = %v", err)
	}
}

func TestCreateVideoAppliesRouteConstraintsAfterKeyEligibilityAndBeforeInputIO(t *testing.T) {
	routes := []model.Route{
		{ID: 1, PublicID: "shared-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video-1.5", Capability: model.CapabilityResponses},
		{ID: 2, PublicID: "shared-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video-1.5", Capability: model.CapabilityVideo},
		{ID: 3, PublicID: "shared-video", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video-1.5", Capability: model.CapabilityVideo},
	}
	jobs := &videoUsageRepository{}
	assets := &videoAssetStoreStub{inputID: "unused", inputData: []byte("unused")}
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(),
		models:     &aliasRouteResolver{byPublic: map[string][]model.Route{"shared-video": routes}},
		clientKeys: clientkeyapp.NewService("test-owner", nil, nil, nil, 60, 4, nil, security.RandomTokenSource{}),
		providers:  providerimpl.NewRegistry(consoleVideoAdmissionAdapter{}),
		mediaJobs:  jobs, mediaAssets: assets,
		logger: slog.Default(),
	}
	service.ConfigureMedia(jobs, mediaapp.NewVideoResources(jobs, nil), 4)
	references := make([]string, provider.ConsoleVideoMaxReferenceImages+1)
	for index := range references {
		references[index] = VideoInputFileReference(fmt.Sprintf("unused_%d", index))
	}
	_, err := service.CreateVideo(context.Background(), VideoInput{
		PublicModel: "shared-video", Prompt: "animate", Duration: 6, Resolution: "720p",
		ReferenceURLs: references,
		ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeRestricted,
			ProviderScope: clientkey.ProviderScopeConsole,
			AllowedModels: []uint64{3},
		},
	})
	if !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("CreateVideo error = %v", err)
	}
	if assets.openInputCalls != 0 || jobs.createCalls != 0 || len(service.background.queue) != 0 || len(service.background.queued) != 0 {
		t.Fatalf("side effects: input opens=%d job creates=%d queue=%d queued-set=%d", assets.openInputCalls, jobs.createCalls, len(service.background.queue), len(service.background.queued))
	}
}

func TestVideo1080pValidationUsesResolvedUpstreamModel(t *testing.T) {
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video-1.5", "1080P", false, 0, 6); err != nil {
		t.Fatalf("1.5 text/image 1080p rejected: %v", err)
	}
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video", "1080p", false, 0, 6); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("legacy 1080p error = %v", err)
	}
	if err := validateVideoRouteParameters(account.ProviderConsole, provider.VideoOperationGenerate, "grok-imagine-video-1.5", "1080p", false, 1, 6); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("reference 1080p error = %v", err)
	}
}

func TestValidateVideoReferenceAudios(t *testing.T) {
	if err := media.ValidateVideoReferenceAudios([]string{"eve", ""}); err == nil {
		t.Fatal("blank reference voice was accepted")
	}
	if err := media.ValidateVideoReferenceAudios([]string{"a", "b", "c", "d"}); err == nil {
		t.Fatal("too many reference voices were accepted")
	}
}

func TestRecoverVideoJobsRecordsFailedAuditWithEgress(t *testing.T) {
	completedAt := time.Now().UTC()
	nodeID := uint64(42)
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_failed_recovery", RequestID: "request-failed-recovery",
		ClientKeyID: 1, ClientKeyName: "client", ClientIP: "203.0.113.9", AccountID: 2, AccountName: "account",
		Provider: "grok_web", Model: "grok-imagine-video", ModelRouteID: 3, UpstreamModel: "video",
		Seconds: 8, Quality: "720p", Status: media.StatusFailed, ErrorCode: "generation_failed", ErrorMessage: "upstream disconnected",
		EgressNodeID: &nodeID, EgressNodeName: "warp", EgressScope: "grok_web", EgressMode: "proxy",
		InputJSON: `{}`, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}}
	recorder := &durableVideoAuditRecorder{}
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), mediaJobs: repository, audits: recorder}
	if err := service.RecoverVideoJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.job.UsageRecordedAt == nil || recorder.calls != 1 {
		t.Fatalf("recordedAt = %v, audit calls = %d", repository.job.UsageRecordedAt, recorder.calls)
	}
	if recorder.last.ClientIP != "203.0.113.9" || recorder.last.StatusCode != 502 || recorder.last.ErrorCode != "generation_failed" || recorder.last.EgressNodeID == nil || *recorder.last.EgressNodeID != nodeID || recorder.last.EgressNodeName != "warp" || recorder.last.EgressMode != audit.EgressModeProxy {
		t.Fatalf("audit = %#v", recorder.last)
	}
	if recorder.last.EstimatedCostInUSDTicks != 0 || recorder.last.MediaOutputSeconds != 0 {
		t.Fatalf("failed job was billed: %#v", recorder.last)
	}
}

func TestRecoverVideoJobsRecordsDetachedAccountSnapshot(t *testing.T) {
	completedAt := time.Now().UTC()
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_detached_account", RequestID: "request-detached-account",
		ClientKeyID: 1, ClientKeyName: "client", AccountName: "deleted account",
		Provider: "grok_web", Model: "grok-imagine-video", ModelRouteID: 3, UpstreamModel: "video",
		Seconds: 8, Quality: "720p", Status: media.StatusFailed, ErrorCode: "generation_failed",
		InputJSON: `{}`, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}}
	recorder := &durableVideoAuditRecorder{}
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), mediaJobs: repository, audits: recorder}
	if err := service.RecoverVideoJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder.last.AccountID != nil || recorder.last.AccountName != "deleted account" {
		t.Fatalf("detached account audit = %#v", recorder.last)
	}
}

func TestLogVideoGenerationFailurePreservesUpstreamDiagnostic(t *testing.T) {
	var output bytes.Buffer
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), logger: slog.New(slog.NewTextHandler(&output, nil))}
	nodeID := uint64(7)
	service.logVideoGenerationFailure(media.Job{
		ID: "video_failure", RequestID: "request-failure", UpstreamModel: "grok-imagine-video",
		EgressNodeID: &nodeID, EgressNodeName: "proxy-1", EgressScope: "grok_web", EgressMode: "proxy",
	}, account.Credential{ID: 42, Provider: account.ProviderWeb}, videoStatusError{
		status:  http.StatusForbidden,
		message: "Grok Web 媒体上游返回 403: upload denied access_token=secret https://assets.grok.com/video?token=secret",
	})
	logLine := output.String()
	for _, expected := range []string{
		"msg=video_generation_failed", "job_id=video_failure", "request_id=request-failure",
		"account_id=42", "provider=grok_web", "upstream_status=403", "upload denied",
		"egress_node_id=7", "egress_node_name=proxy-1",
	} {
		if !strings.Contains(logLine, expected) {
			t.Fatalf("log missing %q: %s", expected, logLine)
		}
	}
	for _, secret := range []string{"access_token=secret", "token=secret"} {
		if strings.Contains(logLine, secret) {
			t.Fatalf("log exposed %q: %s", secret, logLine)
		}
	}
}

type videoStatusError struct {
	status  int
	message string
}

func (e videoStatusError) Error() string       { return e.message }
func (e videoStatusError) HTTPStatusCode() int { return e.status }

func TestVideoQueueIsBoundedAndDeduplicated(t *testing.T) {
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory()}
	service.ConfigureMedia(&videoUsageRepository{}, mediaapp.NewVideoResources(&videoUsageRepository{}, nil), 1)
	capacity := cap(service.background.queue)
	for index := range capacity {
		if !service.background.Enqueue(fmt.Sprintf("video_%d", index)) {
			t.Fatalf("enqueue %d failed before capacity", index)
		}
	}
	if !service.background.Enqueue("video_0") {
		t.Fatal("duplicate queued job should be treated as accepted")
	}
	if service.background.Enqueue("video_overflow") {
		t.Fatal("queue accepted a job beyond its capacity")
	}
}

func TestPersistRemoteVideoRetriesSameResultWithoutRegeneration(t *testing.T) {
	adapter := &videoPersistAdapter{failures: 1}
	store := &videoAssetStoreStub{}
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), mediaAssets: store}
	credential := account.Credential{ID: 42, Provider: account.ProviderWeb}
	result, err := service.persistRemoteVideo(context.Background(), "video_job", adapter, credential, provider.VideoResult{URL: "https://assets.grok.com/video.mp4", ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.generateCalls != 0 || adapter.downloadCalls != 2 || adapter.lastCredentialID != credential.ID {
		t.Fatalf("generate=%d download=%d credential=%d", adapter.generateCalls, adapter.downloadCalls, adapter.lastCredentialID)
	}
	if store.saveCalls != 1 || result.AssetID != "vid_local" || result.ContentType != "video/mp4" {
		t.Fatalf("store calls=%d result=%#v", store.saveCalls, result)
	}
}

func TestResolveVideoInputFileReferenceToDataURI(t *testing.T) {
	raw := []byte("png-bytes")
	inputID := "input_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	store := &videoAssetStoreStub{inputID: inputID, inputData: raw}
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), mediaAssets: store}
	reference := VideoInputFileReference(inputID)
	if err := service.validateVideoInputReferences(context.Background(), []string{reference}, "image"); err != nil {
		t.Fatal(err)
	}
	if err := service.validateVideoInputReferences(context.Background(), []string{reference}, "video"); !errors.Is(err, ErrVideoInputUnavailable) {
		t.Fatalf("image accepted as video input: %v", err)
	}
	resolved, err := service.resolveVideoInputReferences(context.Background(), []string{"https://example.com/a.png", reference}, "image")
	if err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	if len(resolved) != 2 || resolved[0] != "https://example.com/a.png" || resolved[1] != want {
		t.Fatalf("resolved=%#v", resolved)
	}
	if err := service.validateVideoInputReferences(context.Background(), []string{VideoInputFileReference("missing")}, "image"); !errors.Is(err, ErrVideoInputUnavailable) {
		t.Fatalf("missing input error=%v", err)
	}
	store.inputSize = 20 << 20
	if err := service.validateVideoInputReferences(context.Background(), []string{reference, reference}, "image"); !errors.Is(err, ErrVideoInputTooLarge) {
		t.Fatalf("aggregate local input error=%v", err)
	}
}

func TestVideoInputMaterializationHasIndependentBulkhead(t *testing.T) {
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory()}
	service.ConfigureMedia(nil, mediaapp.NewVideoResources(nil, nil), 64)
	reference := VideoInputFileReference("input_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	releases := make([]func(), 0, videoInputMaterializeConcurrency)
	for range videoInputMaterializeConcurrency {
		release, err := service.acquireVideoInputSlot(context.Background(), []string{reference})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.acquireVideoInputSlot(canceled, []string{reference}); !errors.Is(err, context.Canceled) {
		t.Fatalf("fifth local input acquire error=%v", err)
	}
	for _, release := range releases {
		release()
		release() // 释放函数必须可幂等调用。
	}
	if release, err := service.acquireVideoInputSlot(context.Background(), []string{"https://example.com/image.png"}); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

type videoPersistAdapter struct {
	failures         int
	generateCalls    int
	downloadCalls    int
	lastCredentialID uint64
}

type consoleVideoAdmissionAdapter struct{}

func (consoleVideoAdmissionAdapter) Provider() account.Provider { return account.ProviderConsole }

func (consoleVideoAdmissionAdapter) GenerateVideo(context.Context, provider.VideoRequest) (provider.VideoResult, error) {
	return provider.VideoResult{}, errors.New("unexpected Console video generation")
}

func (a *videoPersistAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *videoPersistAdapter) GenerateVideo(context.Context, provider.VideoRequest) (provider.VideoResult, error) {
	a.generateCalls++
	return provider.VideoResult{}, errors.New("must not regenerate")
}

func (a *videoPersistAdapter) DownloadVideo(_ context.Context, credential account.Credential, _ string) (io.ReadCloser, string, int64, error) {
	a.downloadCalls++
	a.lastCredentialID = credential.ID
	if a.downloadCalls <= a.failures {
		return nil, "", 0, errors.New("temporary download failure")
	}
	return io.NopCloser(strings.NewReader("video")), "video/mp4", 5, nil
}

type videoAssetStoreStub struct {
	saveCalls      int
	openInputCalls int
	openAsset      media.Asset
	openData       []byte
	openErr        error
	inputID        string
	inputData      []byte
	inputSize      int64
	inputKind      string
	inputMIME      string
}

func (s *videoAssetStoreStub) SaveVideo(_ context.Context, jobID, contentType string, body io.Reader) (media.Asset, error) {
	s.saveCalls++
	if jobID != "video_job" {
		return media.Asset{}, fmt.Errorf("job ID = %s", jobID)
	}
	if contentType != "video/mp4" {
		return media.Asset{}, fmt.Errorf("content type = %s", contentType)
	}
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "video" {
		return media.Asset{}, fmt.Errorf("video body = %q: %w", data, err)
	}
	return media.Asset{ID: "vid_local", Kind: "video", MIMEType: "video/mp4", SizeBytes: int64(len(data))}, nil
}

func (s *videoAssetStoreStub) OpenVideo(_ context.Context, id string) (media.Asset, io.ReadCloser, error) {
	if s.openErr != nil {
		return media.Asset{}, nil, s.openErr
	}
	if s.openAsset.ID == "" || id != s.openAsset.ID {
		return media.Asset{}, nil, errors.New("not implemented")
	}
	return s.openAsset, io.NopCloser(bytes.NewReader(s.openData)), nil
}

func (s *videoAssetStoreStub) OpenInputAsset(_ context.Context, id string) (media.Asset, io.ReadCloser, error) {
	s.openInputCalls++
	if id != s.inputID || len(s.inputData) == 0 {
		return media.Asset{}, nil, errors.New("not implemented")
	}
	size := s.inputSize
	if size <= 0 {
		size = int64(len(s.inputData))
	}
	kind := s.inputKind
	if kind == "" {
		kind = "image"
	}
	mimeType := s.inputMIME
	if mimeType == "" {
		mimeType = "image/png"
	}
	return media.Asset{ID: id, Kind: kind, MIMEType: mimeType, SizeBytes: size}, io.NopCloser(bytes.NewReader(s.inputData)), nil
}

func (*videoAssetStoreStub) ReleaseInputAssets(context.Context, []string) error { return nil }

type durableVideoAuditRecorder struct {
	failures int
	calls    int
	last     audit.Record
}

func (r *durableVideoAuditRecorder) Create(_ context.Context, value audit.Record) error {
	r.calls++
	r.last = value
	if r.calls <= r.failures {
		return errors.New("database unavailable")
	}
	return nil
}

type videoUsageRepository struct {
	job         media.Job
	createCalls int
}

func (r *videoUsageRepository) CreateMediaJob(_ context.Context, value media.Job) error {
	r.createCalls++
	r.job = value
	return nil
}

func (r *videoUsageRepository) GetMediaJob(context.Context, string, uint64) (media.Job, error) {
	return r.job, nil
}

func (r *videoUsageRepository) GetMediaJobsByIDs(context.Context, []string) ([]media.Job, error) {
	return []media.Job{r.job}, nil
}

func (r *videoUsageRepository) UpdateMediaJob(context.Context, media.Job) error { return nil }

func (r *videoUsageRepository) SaveMediaJobAccessPolicy(_ context.Context, _, _ string, policy media.JobAccessPolicy) error {
	r.job.AccessPolicy = policy
	return nil
}

func (r *videoUsageRepository) SaveMediaJobExecution(_ context.Context, value media.Job, _ media.VideoExecution) error {
	r.job = value
	return nil
}

func (r *videoUsageRepository) DeleteMediaJob(context.Context, string) error { return nil }

func (r *videoUsageRepository) ListMediaJobs(context.Context, repository.MediaJobListQuery) ([]media.Job, int64, error) {
	return nil, 0, nil
}

func (r *videoUsageRepository) SummarizeMediaJobs(context.Context) (repository.MediaJobStats, error) {
	return repository.MediaJobStats{}, nil
}

func (r *videoUsageRepository) ListRecoverableMediaJobs(context.Context, int) ([]media.Job, error) {
	return nil, nil
}

func (r *videoUsageRepository) ListUnrecordedTerminalMediaJobs(context.Context, int) ([]media.Job, error) {
	if r.job.UsageRecordedAt != nil || (r.job.Status != media.StatusCompleted && r.job.Status != media.StatusFailed) {
		return nil, nil
	}
	return []media.Job{r.job}, nil
}

func (r *videoUsageRepository) TryClaimMediaJob(context.Context, string, time.Time, time.Time, string) (media.Job, bool, error) {
	return media.Job{}, false, nil
}

func (r *videoUsageRepository) MarkMediaJobUsageRecorded(_ context.Context, _ string, recordedAt time.Time) error {
	r.job.UsageRecordedAt = &recordedAt
	return nil
}

func TestResolveVideoAuditStatusCodePrefersUpstream429(t *testing.T) {
	status429 := http.StatusTooManyRequests
	job := media.Job{Status: media.StatusFailed, ErrorCode: "generation_failed", ErrorMessage: "Console 媒体上游返回 429: Too many requests"}
	if got := resolveVideoAuditStatusCode(job, 0, nil); got != http.StatusTooManyRequests {
		t.Fatalf("message status = %d", got)
	}
	if got := resolveVideoAuditStatusCode(job, http.StatusTooManyRequests, nil); got != http.StatusTooManyRequests {
		t.Fatalf("explicit status = %d", got)
	}
	attempts := []audit.Attempt{{UpstreamStatusCode: &status429}}
	job.ErrorMessage = "wrapped"
	if got := resolveVideoAuditStatusCode(job, 0, attempts); got != http.StatusTooManyRequests {
		t.Fatalf("attempt status = %d", got)
	}
	job.ErrorCode = "rate_limited"
	job.ErrorMessage = ""
	if got := resolveVideoAuditStatusCode(job, 0, nil); got != http.StatusTooManyRequests {
		t.Fatalf("code status = %d", got)
	}
}

func TestVideoAttemptPolicyStandaloneAndUnlimited(t *testing.T) {
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory()}
	service.UpdateMaxAttempts(3)
	service.UpdateVideoMaxAttempts(0)
	policy := service.videoAttemptPolicy()
	if policy.unlimited || policy.limit != 999 {
		t.Fatalf("legacy zero policy = %#v", policy)
	}
	service.UpdateVideoMaxAttempts(-1)
	policy = service.videoAttemptPolicy()
	if !policy.unlimited {
		t.Fatalf("unlimited video policy = %#v", policy)
	}
	service.UpdateVideoMaxAttempts(5)
	policy = service.videoAttemptPolicy()
	if policy.unlimited || policy.limit != 5 {
		t.Fatalf("standalone policy = %#v", policy)
	}
}

type videoCreateFailoverAdapter struct {
	mu       sync.Mutex
	failures map[uint64]int
	status   int
	attempts []uint64
}

func (a *videoCreateFailoverAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *videoCreateFailoverAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderWeb)
	definition.Media.VideoGeneration = true
	return definition
}

func (a *videoCreateFailoverAdapter) GenerateVideo(_ context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	if err := provider.CheckpointVideo(request, provider.VideoCheckpoint{Phase: media.VideoExecutionSubmitting, Route: "web", Endpoint: "https://grok.invalid"}); err != nil {
		return provider.VideoResult{}, err
	}
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	remaining := a.failures[request.Credential.ID]
	if remaining > 0 {
		a.failures[request.Credential.ID] = remaining - 1
	}
	a.mu.Unlock()
	if remaining > 0 {
		if a.status == 0 {
			return provider.VideoResult{}, errors.New("unclassified create failure")
		}
		if err := provider.CheckpointVideoRejection(request, videoHTTPStatusError{status: a.status}); err != nil {
			return provider.VideoResult{}, err
		}
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageCreate, a.status, videoHTTPStatusError{status: a.status})
	}
	result := provider.VideoResult{AssetID: "video_asset_00001", ContentType: "video/mp4"}
	if err := provider.CheckpointVideo(request, provider.VideoCheckpoint{Phase: media.VideoExecutionGenerated, Result: result}); err != nil {
		return result, err
	}
	return result, nil
}

func (a *videoCreateFailoverAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

type videoHTTPStatusError struct{ status int }

func (e videoHTTPStatusError) Error() string       { return http.StatusText(e.status) }
func (e videoHTTPStatusError) HTTPStatusCode() int { return e.status }

func TestVideoWebForbiddenRetriesPinnedAccountOnceThenFailsOver(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	mediaRepo := relational.NewMediaJobRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	key, err := keyRepo.Create(ctx, clientkey.Key{ModelScope: clientkey.ModelScopeAll,
		Name: "video-test", Prefix: "video-test", SecretHash: strings.Repeat("a", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	createAccount := func(name string, priority int) account.Credential {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
			Name: name, SourceKey: name, EncryptedAccessToken: name + "-token", ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: priority, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return credential
	}
	first := createAccount("first", 200)
	second := createAccount("second", 100)
	if err := testsupport.Discover(ctx, modelRepo, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []uint64{first.ID, second.ID} {
		if err := testsupport.Capabilities(ctx, modelRepo, accountRepo, accountID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}

	adapter := &videoCreateFailoverAdapter{
		failures: map[uint64]int{first.ID: 2},
		status:   http.StatusForbidden,
	}
	registry := providerimpl.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), security.RandomTokenSource{}, nil, nil, nil)
	sel := selector.NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService("test-owner", keyRepo, nil, nil, 60, 4, nil, security.RandomTokenSource{}), registry, sel, nil, security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 3)
	service.ConfigureMedia(mediaRepo, mediaapp.NewVideoResources(mediaRepo, nil), 1)
	service.UpdateVideoMaxAttempts(10)

	now := time.Now().UTC()
	job := media.Job{
		ID: "video_forbidden_retry", RequestID: "request-video-retry", ClientKeyID: key.ID, ClientKeyName: key.Name,
		AccessPolicy: media.JobAccessPolicy{Version: 1, AccountScope: clientkey.AccountScope{Providers: clientkey.ProviderScopeAll, Tiers: clientkey.TierScopeAll}},
		Execution:    media.VideoExecution{Revision: 1, Phase: media.VideoExecutionReady}, ClaimToken: "video_test_execution_claim",
		AccountID: first.ID, AccountName: first.Name, Provider: string(account.ProviderWeb),
		Model: route.PublicID, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
		Operation: provider.VideoOperationGenerate, Prompt: "test", Seconds: 5, Quality: "720p",
		Status: media.StatusInProgress, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := mediaRepo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	service.runVideoJob(ctx, job, route)

	if attempts := adapter.Attempts(); len(attempts) != 3 || attempts[0] != first.ID || attempts[1] != first.ID || attempts[2] != second.ID {
		t.Fatalf("video attempts = %#v, want first, first, second", attempts)
	}
	stored, err := mediaRepo.GetMediaJob(ctx, job.ID, job.ClientKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != media.StatusCompleted || stored.AccountID != second.ID || stored.ResultAssetID != "video_asset_00001" {
		t.Fatalf("completed job = %#v", stored)
	}

	adapter.mu.Lock()
	adapter.failures = map[uint64]int{first.ID: 1}
	adapter.status = 0
	adapter.attempts = nil
	adapter.mu.Unlock()
	unknownJob := job
	unknownJob.ID = "video_unclassified_failure"
	unknownJob.RequestID = "request-video-unclassified"
	unknownJob.AccountID = first.ID
	unknownJob.AccountName = first.Name
	unknownJob.Status = media.StatusInProgress
	unknownJob.Progress = 0
	unknownJob.ResultAssetID = ""
	unknownJob.ContentType = ""
	unknownJob.CompletedAt = nil
	unknownJob.CreatedAt = time.Now().UTC()
	unknownJob.UpdatedAt = unknownJob.CreatedAt
	if err := mediaRepo.CreateMediaJob(ctx, unknownJob); err != nil {
		t.Fatal(err)
	}
	service.runVideoJob(ctx, unknownJob, route)
	if attempts := adapter.Attempts(); len(attempts) != 1 || attempts[0] != first.ID {
		t.Fatalf("unclassified failure attempts = %#v, want pinned account once", attempts)
	}
	stored, err = mediaRepo.GetMediaJob(ctx, unknownJob.ID, unknownJob.ClientKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != media.StatusFailed || stored.AccountID != first.ID {
		t.Fatalf("unclassified failed job = %#v", stored)
	}
}

func (r *videoUsageRepository) StartMediaJobExecutionLimits(_ context.Context, _, _ string, limits media.ExecutionLimits) error {
	r.job.Limits = limits
	return nil
}
func (r *videoUsageRepository) ReserveMediaJobPhysicalCall(context.Context, string, string, time.Time) error {
	r.job.Limits.Reserved++
	return nil
}
func (r *videoUsageRepository) ConfirmMediaJobPhysicalCalls(_ context.Context, _, _ string, _, confirmed uint32) error {
	r.job.Limits.Confirmed = confirmed
	return nil
}

func (r *videoUsageRepository) ListUnrecordedMediaJobQuotas(_ context.Context, afterID string, _ int) ([]media.Job, error) {
	// This fixture isolates audit replay. Durable quota recovery is exercised
	// with the real job/account repositories in the HTTP integration suite.
	return nil, nil
}
func (r *videoUsageRepository) MarkMediaJobQuotaRecorded(_ context.Context, _ media.Job, now time.Time) error {
	r.job.Quota.RecordedAt = &now
	return nil
}
