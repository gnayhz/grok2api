package court

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestLiveCaseWaitingUsesSavedExperimentPolicy(t *testing.T) {
	bench := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{bench.evidence}, simpleTaskDispatcher{registry.NewProbeTaskStore(bench.registry)})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	id := openSimpleTestCase(t, service, bench.registry)
	first := true
	settleSimpleTestTasks(t, bench.registry, id, func(task registry.ProbeTaskView) model.ProbeTaskResult {
		if task.Direction == model.ProbeAccountDifferential && first {
			first = false
			return model.ProbeTaskResult{Outcome: model.ProbeResultClean}
		}
		return model.ProbeTaskResult{Outcome: model.ProbeResultError, Detail: "network_unavailable"}
	})
	before, err := service.LiveCaseViews(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].WaitingReasonCode != "replacing_failed_exits" {
		t.Fatalf("before=%+v", before)
	}
	cfg.AccountNeedExits = 1
	cfg.AccountSpanNodes = 1
	service.SetConfig(cfg)
	after, err := service.LiveCaseViews(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].WaitingReasonCode != before[0].WaitingReasonCode || after[0].WaitingReason != before[0].WaitingReason || after[0].AccountNeedExits != before[0].AccountNeedExits {
		t.Fatalf("frozen case projection changed: before=%+v after=%+v", before, after)
	}
}
