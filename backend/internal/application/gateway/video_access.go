package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

// videoAccountScope restores the accepted request grant before input IO or
// account selection. Legacy jobs must acquire an explicit, durable grant from
// the current Key; missing policy never implies permission to use every tier.
func (s *Service) videoAccountScope(ctx context.Context, job *media.Job, route model.Route) (clientkey.AccountScope, error) {
	policy := job.AccessPolicy
	if policy.IsLegacy() {
		if s.clientKeys == nil {
			return clientkey.AccountScope{}, fmt.Errorf("视频任务缺少可恢复的客户端授权")
		}
		key, err := s.clientKeys.Get(ctx, job.ClientKeyID)
		if err != nil {
			return clientkey.AccountScope{}, fmt.Errorf("读取视频任务客户端授权: %w", err)
		}
		if !key.IsAvailable(time.Now().UTC()) || !s.clientKeys.CanUseModel(key, route.ID) || !key.AccountScope().AllowsProvider(route.Provider) {
			return clientkey.AccountScope{}, fmt.Errorf("视频任务客户端授权已不可用")
		}
		policy, err = media.NewJobAccessPolicy(key.AccountScope())
		if err != nil {
			return clientkey.AccountScope{}, err
		}
		if err := s.mediaJobs.SaveMediaJobAccessPolicy(ctx, job.ID, job.ClaimToken, policy); err != nil {
			return clientkey.AccountScope{}, fmt.Errorf("保存视频任务客户端授权: %w", err)
		}
		job.AccessPolicy = policy
	}
	scope, err := policy.Scope()
	if err != nil {
		return clientkey.AccountScope{}, err
	}
	if !scope.AllowsProvider(route.Provider) {
		return clientkey.AccountScope{}, fmt.Errorf("视频任务授权不允许使用此Provider")
	}
	return scope, nil
}
