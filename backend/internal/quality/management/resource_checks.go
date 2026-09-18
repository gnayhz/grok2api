package management

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

type ResourceCheckStore interface {
	CreateResourceCheck(context.Context, model.ProbeTask, int) (uint64, error)
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

type CheckSubmission struct {
	ResourceID uint64 `json:"resource_id"`
	ID         uint64 `json:"id,omitempty"`
	Error      string `json:"error,omitempty"`
}

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
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
	items := make([]CheckSubmission, 0, len(ids))
	seen := map[uint64]bool{}
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
		sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
		plan := &model.ResourceCheckPlan{Kind: kind, ResourceID: id, Accounts: []uint64{}, Nodes: []uint64{}}
		if kind == "node" && !fixed[id] {
			plan.UnavailableReason = "path_unverified"
		}
		for _, account := range accounts {
			if kind == "account" && account == id {
				continue
			}
			plan.Accounts = append(plan.Accounts, account)
			if len(plan.Accounts) == 8 {
				break
			}
		}
		for _, node := range nodes {
			if kind == "node" && node == id {
				continue
			}
			plan.Nodes = append(plan.Nodes, node)
			if len(plan.Nodes) == model.ResourceCheckMaxGroups {
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
		item.ID, err = s.store.CreateResourceCheck(ctx, task, model.AccountCheckQueueLimit)
		if err != nil {
			item.Error = "submission_failed"
			if errors.Is(err, model.ErrCheckQueueFull) {
				item.Error = "queue_full"
			}
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *ResourceChecks) List(ctx context.Context, kind string, ids []uint64) ([]model.ResourceCheck, error) {
	if !validResources(kind, ids) {
		return nil, errors.New("invalid resource query")
	}
	return s.store.ListResourceChecks(ctx, kind, ids)
}
