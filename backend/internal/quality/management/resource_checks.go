package management

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

// Read results exposed by this use case. Transport can encode these values
// without importing the model package's evidence and attribution policy.
type ResourceCheck = model.ResourceCheck
type ResourceCheckReport = model.ResourceCheckReport
type ResourceSample = model.ResourceSample

type ResourceCheckStore interface {
	CreateResourceCheckBatch(context.Context, []model.ProbeTask, int) ([]model.ResourceSubmission, error)
	ListResourceChecks(context.Context, string, []uint64) ([]model.ResourceCheck, error)
}
type ResourceCheckPreparer interface {
	PrepareResourceCheck(context.Context, string, uint64, string) (model.ProbeExperiment, error)
	QualityProbeAccounts(context.Context, model.ProbeExperiment) ([]uint64, error)
}
type ResourceChecks struct {
	store   ResourceCheckStore
	prepare ResourceCheckPreparer
	nodes   proxy.NodeSource
}

func NewResourceChecks(store ResourceCheckStore, prepare ResourceCheckPreparer, nodes proxy.NodeSource) *ResourceChecks {
	return &ResourceChecks{store: store, prepare: prepare, nodes: nodes}
}

type CheckSubmission = model.ResourceSubmission
type ResourceObservation = model.ResourceObservation
type ResourceProof = model.ResourceProof

func validResources(kind string, ids []uint64) bool {
	if kind != "account" && kind != "node" || len(ids) == 0 || len(ids) > 32 {
		return false
	}
	for _, id := range ids {
		if id == 0 {
			return false
		}
	}
	return true
}

// Each item is durable as soon as accepted. Partial submission is explicit;
// retrying finds the existing active task instead of launching duplicate probes.
func (s *ResourceChecks) Start(ctx context.Context, kind string, ids []uint64, publicModel string) ([]CheckSubmission, error) {
	publicModel = strings.TrimSpace(publicModel)
	if !validResources(kind, ids) || publicModel == "" || len(publicModel) > 200 {
		return nil, errors.New("invalid resource check request")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	profiles, err := s.nodes.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	nodes := []uint64{}
	enabled := map[uint64]bool{}
	fixed := map[uint64]bool{}
	for _, p := range profiles {
		enabled[p.ID] = p.Enabled
		if p.Enabled && p.CanServeFixedTarget && !p.ProxyPool {
			fixed[p.ID] = true
			nodes = append(nodes, p.ID)
		}
	}
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
	items := make([]CheckSubmission, 0, len(ids))
	seen := map[uint64]bool{}
	tasks := []model.ProbeTask{}
	positions := []int{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		item := CheckSubmission{ResourceID: id}
		if kind == "node" && !enabled[id] {
			item.Error = "resource_unavailable"
			items = append(items, item)
			continue
		}
		spec, err := s.prepare.PrepareResourceCheck(ctx, kind, id, publicModel)
		if err != nil {
			item.Error = "resource_unavailable"
			items = append(items, item)
			continue
		}
		accounts, err := s.prepare.QualityProbeAccounts(ctx, spec)
		if err != nil {
			item.Error = "candidates_unavailable"
			items = append(items, item)
			continue
		}
		rand.Shuffle(len(accounts), func(i, j int) { accounts[i], accounts[j] = accounts[j], accounts[i] })
		plan := &model.ResourceCheckPlan{Kind: kind, ResourceID: id, Accounts: []uint64{}, Nodes: []uint64{}}
		if kind == "node" && !fixed[id] {
			plan.UnavailableReason = "path_unverified"
		}
		for _, account := range accounts {
			if kind == "account" && account == id {
				continue
			}
			plan.Accounts = append(plan.Accounts, account)
			if len(plan.Accounts) == model.ResourceCheckMaxAccounts {
				break
			}
		}
		for _, node := range nodes {
			if kind == "node" && node == id {
				continue
			}
			plan.Nodes = append(plan.Nodes, node)
			if len(plan.Nodes) == model.ResourceCheckMaxNodes {
				break
			}
		}
		spec.ResourceCheck = plan
		task := model.ProbeTask{Direction: model.ProbeResourceCheck, Experiment: spec}
		if kind == "account" {
			task.DefendantAccountID = id
		} else {
			task.DefendantNodeID = id
		}
		positions = append(positions, len(items))
		tasks = append(tasks, task)
		items = append(items, item)
	}
	if len(tasks) > 0 {
		accepted, err := s.store.CreateResourceCheckBatch(ctx, tasks, model.ResourceCheckQueueLimit)
		if err != nil {
			for _, pos := range positions {
				items[pos].Error = "submission_failed"
			}
		} else {
			for i, item := range accepted {
				items[positions[i]] = item
			}
		}
	}
	return items, nil
}

func (s *ResourceChecks) List(ctx context.Context, kind string, ids []uint64) ([]ResourceCheck, error) {
	if !validResources(kind, ids) {
		return nil, errors.New("invalid resource query")
	}
	return s.store.ListResourceChecks(ctx, kind, ids)
}
