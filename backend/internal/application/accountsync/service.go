package accountsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

const (
	defaultWorkerCount = 25
	operationTimeout   = 2 * time.Minute
)

type billingSynchronizer interface {
	HasBillingSnapshot(ctx context.Context, accountID uint64) (bool, error)
	RefreshBilling(ctx context.Context, accountID uint64) (accountdomain.Billing, error)
}

type modelSynchronizer interface {
	HasSuccessfulAccountSync(ctx context.Context, accountID uint64) (bool, error)
	SyncAccount(ctx context.Context, accountID uint64) (int, error)
}

type accountReader interface {
	Get(ctx context.Context, id uint64) (accountapp.View, error)
}

type providerPolicy interface {
	ProviderDefinition(value accountdomain.Provider) (provider.Definition, bool)
}

type quotaSynchronizer interface {
	HasQuotaWindows(ctx context.Context, accountID uint64) (bool, error)
	RefreshQuota(ctx context.Context, accountID uint64) ([]accountdomain.QuotaWindow, error)
}

type identitySynchronizer interface {
	SyncAccountIdentity(ctx context.Context, accountID uint64) error
}

type deviceLoginSource interface {
	PollDeviceLogin(ctx context.Context, sessionID string) (accountapp.View, error)
}

type accountAdministrator interface {
	Update(ctx context.Context, id uint64, input accountapp.UpdateInput) (accountapp.View, error)
}

// Service 对新接入账号执行一次性额度与模型补齐，并限制批量同步并发。
type Service struct {
	logger   *slog.Logger
	accounts accountReader
	// policy 是必需依赖：账号生命周期策略在构造时显式声明，
	// 不再通过运行时类型断言发现。
	policy providerPolicy
	// identity 是可选能力：nil 表示组合根未装配身份同步；装配后
	// 才会调用，Provider 支持与否不再静默影响同步行为。
	identity identitySynchronizer
	// device 完成 Device OAuth 轮询并返回已提交凭据视图。
	device deviceLoginSource
	// admin 提供账号管理更新命令（update-with-model-sync 用例）。
	admin    accountAdministrator
	billing  billingSynchronizer
	quota    quotaSynchronizer
	models   modelSynchronizer
	syncs    accountapp.OperationGroup[uint64]
	workers  atomic.Int64
	bulkPool *batch.Pool
}

func NewService(logger *slog.Logger, accounts accountReader, policy providerPolicy, identity identitySynchronizer, device deviceLoginSource, admin accountAdministrator, billing billingSynchronizer, quota quotaSynchronizer, models modelSynchronizer) *Service {
	if policy == nil {
		panic("accountsync: provider lifecycle policy 不能为空")
	}
	service := &Service{logger: logger, accounts: accounts, policy: policy, identity: identity, device: device, admin: admin, billing: billing, quota: quota, models: models, bulkPool: batch.NewPool(defaultWorkerCount)}
	service.workers.Store(defaultWorkerCount)
	return service
}

func (s *Service) SetBulkPool(pool *batch.Pool) {
	if pool != nil {
		s.bulkPool = pool
	}
}

func (s *Service) UpdateConcurrency(value int) {
	if value < 1 {
		value = defaultWorkerCount
	}
	s.workers.Store(int64(value))
	s.bulkPool.UpdateLimit(value)
}

// SyncModels 强制刷新指定账号的模型能力，不受初始同步快照跳过规则影响。
func (s *Service) SyncModels(ctx context.Context, accountID uint64) error {
	if accountID == 0 {
		return errors.New("账号 ID 无效")
	}
	if s.models == nil {
		return errors.New("模型同步器未初始化")
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	_, err := s.models.SyncAccount(operationCtx, accountID)
	if err != nil {
		s.logger.Warn("account_model_sync_failed", "account_id", accountID, "error", err)
		return fmt.Errorf("同步模型: %w", err)
	}
	return nil
}

// Result 汇总本次初始同步成功与失败的账号数。
type Result struct {
	Succeeded int
	Failed    int
}

// Sync 等待本次涉及的账号完成额度与模型补齐；已同步数据会跳过，同账号并发请求会合并。
func (s *Service) Sync(ctx context.Context, accountIDs ...uint64) Result {
	input := make(chan uint64, len(accountIDs))
	for _, accountID := range accountIDs {
		input <- accountID
	}
	close(input)
	return s.SyncStream(ctx, input)
}

// SyncStream 以固定 Worker 数消费持续到达的账号，使导入与上游同步形成有界流水线。
func (s *Service) SyncStream(ctx context.Context, accountIDs <-chan uint64) Result {
	return s.syncStream(ctx, accountIDs, nil)
}

// SyncStreamObserved 在每个去重账号完成初始同步后报告进度。
func (s *Service) SyncStreamObserved(ctx context.Context, accountIDs <-chan uint64, observer func(completed, total int)) Result {
	return s.syncStream(ctx, accountIDs, observer)
}

func (s *Service) syncStream(ctx context.Context, accountIDs <-chan uint64, observer func(completed, total int)) Result {
	jobs := make(chan uint64)
	var workers sync.WaitGroup
	var succeeded atomic.Int64
	var failed atomic.Int64
	var total atomic.Int64
	var progressMu sync.Mutex
	completed := 0
	count := max(1, int(s.workers.Load()))
	workers.Add(count)
	for range count {
		go func() {
			defer workers.Done()
			for accountID := range jobs {
				err := s.bulkPool.Do(ctx, func(workCtx context.Context) error {
					_, syncErr := s.syncs.Do(workCtx, accountID, func() (any, error) {
						return nil, s.syncAccount(workCtx, accountID)
					})
					return syncErr
				})
				if err != nil {
					var panicErr *batch.PanicError
					if errors.As(err, &panicErr) {
						s.logger.Error("account_initial_sync_panicked", "account_id", accountID, "error", panicErr, "stack", string(panicErr.Stack))
					}
					failed.Add(1)
				} else {
					succeeded.Add(1)
				}
				if observer != nil {
					progressMu.Lock()
					completed++
					observer(completed, int(total.Load()))
					progressMu.Unlock()
				}
			}
		}()
	}
	seen := make(map[uint64]struct{})
sendLoop:
	for {
		select {
		case <-ctx.Done():
			break sendLoop
		case accountID, ok := <-accountIDs:
			if !ok {
				break sendLoop
			}
			if accountID == 0 {
				continue
			}
			if _, exists := seen[accountID]; exists {
				continue
			}
			seen[accountID] = struct{}{}
			total.Add(1)
			select {
			case jobs <- accountID:
			case <-ctx.Done():
				break sendLoop
			}
		}
	}
	close(jobs)
	workers.Wait()
	return Result{Succeeded: int(succeeded.Load()), Failed: int(failed.Load())}
}

func (s *Service) syncAccount(ctx context.Context, accountID uint64) error {
	var syncErr error
	billingSnapshotCreated := false
	view, err := s.accounts.Get(ctx, accountID)
	if err != nil {
		return fmt.Errorf("读取账号: %w", err)
	}
	definition, ok := s.policy.ProviderDefinition(view.Credential.Provider)
	if !ok {
		return fmt.Errorf("Provider %s 未注册生命周期策略", view.Credential.Provider)
	}
	if view.Credential.Provider == accountdomain.ProviderWeb || view.Credential.Provider == accountdomain.ProviderConsole {
		if s.identity != nil {
			operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			identityErr := s.identity.SyncAccountIdentity(operationCtx, accountID)
			if identityErr != nil {
				s.logger.Warn("account_initial_identity_sync_failed", "account_id", accountID, "error", identityErr)
			}
			cancel()
			if errors.Is(identityErr, provider.ErrUnauthorized) {
				return fmt.Errorf("同步账号身份: %w", identityErr)
			}
		}
	}
	if definition.Quota == provider.QuotaRemoteWindow || definition.Quota == provider.QuotaLocalWindow {
		hasQuota, quotaErr := s.quota.HasQuotaWindows(ctx, accountID)
		if quotaErr != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("检查 Provider 额度快照: %w", quotaErr))
		} else if !hasQuota {
			operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			_, quotaErr = s.quota.RefreshQuota(operationCtx, accountID)
			cancel()
			if quotaErr != nil {
				syncErr = errors.Join(syncErr, fmt.Errorf("同步 Provider 额度: %w", quotaErr))
			}
		}
	} else {
		hasBilling, err := s.billing.HasBillingSnapshot(ctx, accountID)
		if err != nil {
			s.logger.Warn("account_initial_billing_check_failed", "account_id", accountID, "error", err)
			syncErr = errors.Join(syncErr, fmt.Errorf("检查额度快照: %w", err))
		} else if !hasBilling {
			operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			_, err = s.billing.RefreshBilling(operationCtx, accountID)
			cancel()
			if err != nil {
				s.logger.Warn("account_initial_billing_sync_failed", "account_id", accountID, "error", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("同步额度: %w", err))
			} else {
				billingSnapshotCreated = true
			}
		}
	}

	hasModels, err := s.models.HasSuccessfulAccountSync(ctx, accountID)
	if err != nil {
		s.logger.Warn("account_initial_model_check_failed", "account_id", accountID, "error", err)
		return errors.Join(syncErr, fmt.Errorf("检查模型快照: %w", err))
	}
	if hasModels && !billingSnapshotCreated {
		return syncErr
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	_, err = s.models.SyncAccount(operationCtx, accountID)
	cancel()
	if err != nil {
		s.logger.Warn("account_initial_model_sync_failed", "account_id", accountID, "error", err)
		syncErr = errors.Join(syncErr, fmt.Errorf("同步模型: %w", err))
	}
	return syncErr
}

// DeviceCompletion 是 Device 接入完成后的可观察结果：已提交账号与初始
// 同步事实分开表达，同步部分失败不遮蔽已提交账号。
type DeviceCompletion struct {
	Account accountapp.View
	Sync    Result
}

// CompleteDeviceLogin 完成 Device OAuth 的最后一步：轮询已发放凭据、为已
// 提交账号执行初始额度/模型补齐并读回已提交视图。pending/slowdown/denied
// 仍以原错误返回，由传输层映射状态；HTTP 不再编排跨用例步骤。
func (s *Service) CompleteDeviceLogin(ctx context.Context, sessionID string) (DeviceCompletion, error) {
	value, err := s.device.PollDeviceLogin(ctx, sessionID)
	if err != nil {
		return DeviceCompletion{}, err
	}
	sync := s.Sync(ctx, value.Credential.ID)
	if refreshed, refreshErr := s.accounts.Get(ctx, value.Credential.ID); refreshErr == nil {
		value = refreshed
	}
	return DeviceCompletion{Account: value, Sync: sync}, nil
}

// UpdateWithSyncResult 是账号更新带模型补齐的可观察结果：更新已提交的
// 账号与模型同步失败事实分开，补齐失败不回滚也不遮蔽已提交更新。
type UpdateWithSyncResult struct {
	Account         accountapp.View
	ModelSyncFailed bool
}

// UpdateWithModelSync 更新账号并在 Build 授权标记可能变化后补齐模型能力。
// 是否需要补齐由本用例依据输入事实决定，HTTP 不再分支。
func (s *Service) UpdateWithModelSync(ctx context.Context, id uint64, input accountapp.UpdateInput) (UpdateWithSyncResult, error) {
	value, err := s.admin.Update(ctx, id, input)
	if err != nil {
		return UpdateWithSyncResult{}, err
	}
	result := UpdateWithSyncResult{Account: value}
	if input.BuildSuperEntitled != nil {
		result.ModelSyncFailed = s.SyncModels(ctx, id) != nil
	}
	return result, nil
}
