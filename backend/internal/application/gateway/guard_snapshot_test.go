package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type changingGuardSource struct{ value atomic.Pointer[GuardSnapshot] }

func (s *changingGuardSource) GuardSnapshot() GuardSnapshot { return *s.value.Load() }

type rejectAllKernel struct{}

func (rejectAllKernel) ClassifyQualityHold(QualityStreamSignals) QualityVerdict {
	return QualityWithhold
}

func TestRequestGuardSnapshotPinsPolicyAndKernelAcrossUpdates(t *testing.T) {
	source := &changingGuardSource{}
	source.value.Store(&GuardSnapshot{Runtime: normalizeQualityRetry(QualityRetryRuntime{Enabled: true, Revision: 7, MaxAttempts: 2, GuardedModels: []string{"grok-4.6"}}), Kernel: builtinQualityKernel{}})
	service := &Service{}
	service.SetGuardSnapshotSource(source)
	request, scope := service.requestGuardSnapshot()
	source.value.Store(&GuardSnapshot{Runtime: normalizeQualityRetry(QualityRetryRuntime{Enabled: false, Revision: 8, MaxAttempts: 9, GuardedModels: []string{"other"}}), Kernel: rejectAllKernel{}})
	service.UpdateQualityRetry(QualityRetryRuntime{Enabled: false, MaxAttempts: 20})
	if request.Revision != 7 || request.MaxAttempts != 2 || !request.Enabled || !scope.Jurisdiction("grok_build", "grok-4.6") {
		t.Fatalf("mixed policy: %+v", request)
	}
	body := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\n"
	replay, verdict, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(body)), qualityProtocolResponses, request)
	defer replay.Close()
	if err != nil || verdict != QualityDeliver {
		t.Fatalf("old request used new kernel: %v/%v", verdict, err)
	}
	next, _ := service.requestGuardSnapshot()
	if next.Revision != 8 || next.Enabled || next.classify(QualityStreamSignals{HasThinking: true}) != QualityWithhold {
		t.Fatal("new request missed new policy")
	}
	other := &Service{}
	other.UpdateQualityRetry(QualityRetryRuntime{Enabled: true})
	isolated, _ := other.requestGuardSnapshot()
	if isolated.classify(QualityStreamSignals{HasThinking: true}) != QualityDeliver {
		t.Fatal("kernel leaked across instances")
	}
}

func TestProtectedRequestFailsBeforeUpstreamWhenKernelUnavailable(t *testing.T) {
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	service, _ := newGuardLoopService(t, adapter, "unavailable")
	source := &changingGuardSource{}
	source.value.Store(&GuardSnapshot{Runtime: normalizeQualityRetry(QualityRetryRuntime{Enabled: true, GuardedModels: []string{"grok-4.6"}})})
	service.SetGuardSnapshotSource(source)
	result, err := service.CreateChatCompletion(context.Background(), guardLoopInput("unavailable", true))
	var failure *UpstreamFailure
	if result != nil || !errors.As(err, &failure) || failure.HTTPStatus != http.StatusServiceUnavailable || failure.Code != "quality_guard_unavailable" {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestMalformedAuthoritySnapshotRejectsBeforeUpstream(t *testing.T) {
	for _, mutate := range []func(*QualityRetryRuntime){
		func(c *QualityRetryRuntime) { c.GuardedModels = []string{"unknown:grok-4.6"} },
		func(c *QualityRetryRuntime) { c.GuardedModels = []string{""} },
		func(c *QualityRetryRuntime) { c.Enabled = false; c.EvidenceTimeout = -1 },
		func(c *QualityRetryRuntime) { c.AdmissionTimeout = 0 },
	} {
		adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
		service, _ := newGuardLoopService(t, adapter, "invalid-authority")
		runtime := normalizeQualityRetry(QualityRetryRuntime{Enabled: true, GuardedModels: []string{"grok-4.6"}})
		mutate(&runtime)
		source := &changingGuardSource{}
		source.value.Store(&GuardSnapshot{Runtime: runtime, Kernel: builtinQualityKernel{}})
		service.SetGuardSnapshotSource(source)
		result, err := service.CreateChatCompletion(context.Background(), guardLoopInput("invalid-authority", true))
		var failure *UpstreamFailure
		if result != nil || !errors.As(err, &failure) || failure.Code != "quality_guard_unavailable" || len(adapter.Attempts()) != 0 {
			t.Fatalf("invalid authority reached execution: result=%v err=%v attempts=%v", result, err, adapter.Attempts())
		}
	}
}

func BenchmarkRequestGuardSnapshot(b *testing.B) {
	source := &changingGuardSource{}
	source.value.Store(&GuardSnapshot{Runtime: normalizeQualityRetry(QualityRetryRuntime{Enabled: true, Revision: 7, MaxAttempts: 2, GuardedModels: []string{"grok-4.5", "grok-4.6"}}), Kernel: builtinQualityKernel{}})
	service := &Service{}
	service.SetGuardSnapshotSource(source)
	b.ReportAllocs()
	for b.Loop() {
		_, scope := service.requestGuardSnapshot()
		if !scope.Jurisdiction("grok_build", "grok-4.6") {
			b.Fatal("lost jurisdiction")
		}
	}
}
