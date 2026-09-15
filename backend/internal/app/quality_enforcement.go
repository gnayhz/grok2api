package app

import (
	"context"
	"errors"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	qualityenforcement "github.com/chenyme/grok2api/backend/internal/quality/enforcement"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// baseNodeSource translates M14's credential-free facts into M16's profile.
// Pool subtype has no separate data-plane field; preserve the sticky profile.
type baseNodeSource struct{ egress *egressapp.Service }

func (s baseNodeSource) ListProfiles(ctx context.Context) ([]proxy.NodeProfile, error) {
	facts, err := s.egress.ListNodeFacts(ctx)
	if err != nil {
		return nil, err
	}
	profiles := make([]proxy.NodeProfile, 0, len(facts))
	for _, fact := range facts {
		profiles = append(profiles, profileFromFact(fact))
	}
	return profiles, nil
}

func (s baseNodeSource) Profile(ctx context.Context, nodeID uint64) (proxy.NodeProfile, bool, error) {
	fact, found, err := s.egress.NodeFacts(ctx, nodeID)
	return profileFromFact(fact), found, err
}

func profileFromFact(fact egressdomain.NodeFacts) proxy.NodeProfile {
	return proxy.NodeProfile{ID: fact.ID, Name: fact.Name, Enabled: fact.Enabled,
		ProxyPool: fact.ProxyPool, RotationWebhook: fact.RotationEnabled, PoolSticky: fact.ProxyPool,
		CanServeFixedTarget: fact.CanServeFixedTarget, CooldownUntil: fact.CooldownUntil}
}

// IP observations are passive: only the transport owner's persisted revision
// can advance an epoch. A source failure is neither absence nor a new identity.
type baseExitIPSource struct{ egress *egressapp.Service }

func (s baseExitIPSource) CurrentExitIdentity(ctx context.Context, nodeID uint64) (qualitymodel.ExitIdentity, uint64, bool, error) {
	fact, found, err := s.egress.NodeFacts(ctx, nodeID)
	if err != nil {
		return qualitymodel.ExitIdentity{}, 0, false, err
	}
	if !found || !fact.Enabled || fact.ProbeRevision == 0 {
		return qualitymodel.ExitIdentity{}, 0, false, nil
	}
	identity := qualitymodel.ExitIdentity{IPv4: fact.ExitIPv4, IPv6: fact.ExitIPv6}
	if !identity.Present() && fact.ExitIP != "" {
		// 分族探活列缺失时(旧数据/旧写入方),按地址形态从聚合列归族,
		// 保持旧观测口径可继续推进身份;首次观测到缺失的另一族时由
		// 登记处采纳补写基线。
		identity = qualitymodel.ExitIdentityFromAggregate(fact.ExitIP)
	}
	if !identity.Present() {
		return qualitymodel.ExitIdentity{}, 0, false, nil
	}
	return identity, fact.ProbeRevision, true, nil
}

// baseRotator adapts quality commands to the network execution owner; queue,
// retries and shared capacity remain in egress.Service.
type baseRotator struct {
	egress *egressapp.Service
}

func (r baseRotator) TriggerRotation(ctx context.Context, nodeID uint64) error {
	return rotationCommandError(r.egress.RotateNode(ctx, nodeID))
}

func (r baseRotator) TriggerAutomaticRotation(ctx context.Context, nodeID uint64) error {
	return rotationCommandError(r.egress.RotateNodeAutomatically(ctx, nodeID))
}

// bootstrapEnforcementLayer creates the evaluator's owned epoch polling loop.
// Application closes it before releasing network and persistence dependencies.
func bootstrapEnforcementLayer(qualityRegistry *qualityregistry.Registry, egressService *egressapp.Service) *qualityenforcement.Service {
	service := qualityenforcement.New(qualityenforcement.DefaultConfig(), qualityRegistry,
		baseNodeSource{egress: egressService}, baseExitIPSource{egress: egressService}, baseRotator{egress: egressService})
	return service
}

func rotationCommandError(err error) error {
	if errors.Is(err, egressapp.ErrRotationQueueFull) {
		return errors.Join(qualityenforcement.ErrRateLimited, err)
	}
	return err
}
