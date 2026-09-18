package management

import (
	"context"
	"fmt"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

type resourceCheckFixture struct{ tasks []model.ProbeTask }

func (f *resourceCheckFixture) CreateResourceCheck(_ context.Context, task model.ProbeTask, _ int) (uint64, error) {
	if task.DefendantAccountID == 9 {
		return 0, model.ErrCheckQueueFull
	}
	f.tasks = append(f.tasks, task)
	return uint64(len(f.tasks)), nil
}
func (f *resourceCheckFixture) ListResourceChecks(context.Context, string, []uint64) ([]model.ResourceCheck, error) {
	return []model.ResourceCheck{}, nil
}
func (f *resourceCheckFixture) PrepareResourceCheck(_ context.Context, kind string, id uint64, _ string) (model.ProbeExperiment, error) {
	if id == 8 {
		return model.ProbeExperiment{}, fmt.Errorf("fictional unavailable")
	}
	return model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}, nil
}
func (f *resourceCheckFixture) QualityProbeAccounts(context.Context, model.ProbeExperiment) ([]uint64, error) {
	return []uint64{1, 2, 3}, nil
}
func (f *resourceCheckFixture) ListProfiles(context.Context) ([]proxy.NodeProfile, error) {
	return []proxy.NodeProfile{{ID: 11, Enabled: true, CanServeFixedTarget: true}, {ID: 12, Enabled: true, ProxyPool: true, CanServeFixedTarget: true}}, nil
}
func (f *resourceCheckFixture) Profile(context.Context, uint64) (proxy.NodeProfile, bool, error) {
	return proxy.NodeProfile{}, false, nil
}

func TestResourceCheckBatchReportsPartialAcceptanceAndFiltersCandidates(t *testing.T) {
	f := &resourceCheckFixture{}
	service := NewResourceChecks(f, f, f)
	items, err := service.Start(context.Background(), "account", []uint64{1, 1, 8, 9}, "fictional-model")
	if err != nil || len(items) != 3 || items[0].ID == 0 || items[1].Error != "resource_unavailable" || items[2].Error != "queue_full" {
		t.Fatalf("%+v %v", items, err)
	}
	plan := f.tasks[0].Experiment.ResourceCheck
	if len(plan.Accounts) != 2 || plan.Accounts[0] != 2 || len(plan.Nodes) != 1 || plan.Nodes[0] != 11 {
		t.Fatalf("%+v", plan)
	}
	items, err = service.Start(context.Background(), "node", []uint64{12}, "fictional-model")
	if err != nil || items[0].ID == 0 || f.tasks[1].Experiment.ResourceCheck.UnavailableReason != "path_unverified" {
		t.Fatal("rotating exit misrepresented", items, err)
	}
	if _, err = service.Start(context.Background(), "account", make([]uint64, 33), "fictional-model"); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}
