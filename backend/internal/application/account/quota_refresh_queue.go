package account

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type quotaRefreshState struct {
	generation          uint64
	publishedGeneration uint64
	sharedVersion       repository.QuotaRefreshVersion
	runningGeneration   uint64
	runningVersion      repository.QuotaRefreshVersion
	parkedUntil         time.Time
	queued              bool
	running             bool
	pending             bool
	durable             bool
	durableSeenScan     uint64
	failures            int
	nextAttemptAt       time.Time
}

type quotaRefreshRequest struct {
	key       string
	accountID uint64
	mode      string
}

// ready is shared by explicit demand, durable/shared recovery and worker claim.
func (s *quotaRefreshState) ready(now time.Time) bool {
	return s.pending && !s.queued && !s.running && s.failures < quotaRefreshFailureBudget && !now.Before(s.nextAttemptAt)
}

func (s *quotaRefreshState) canAdopt(version repository.QuotaRefreshVersion, now time.Time) bool {
	return version.Generation != 0 && now.Before(version.ExpiresAt) &&
		(s.sharedVersion.Generation == 0 || !now.Before(s.sharedVersion.ExpiresAt) || version.Generation > s.sharedVersion.Generation)
}

// Repeated observations do not start a new retry episode. Expiry disambiguates
// counters that restart at one after the coordinator retires an old signal.
func (s *quotaRefreshState) observe(version repository.QuotaRefreshVersion, now time.Time) {
	if version.Generation == 0 || !now.Before(version.ExpiresAt) {
		return
	}
	if !version.Equal(s.sharedVersion) {
		if !s.canAdopt(version, now) {
			return
		}
		s.sharedVersion = version
		s.failures = 0
		s.parkedUntil = time.Time{}
	}
	if s.failures < quotaRefreshFailureBudget {
		s.pending = true
	}
}

// QueueQuotaRefresh asynchronously refreshes the remote quota window after a successful request.
func (s *Service) QueueQuotaRefresh(id uint64, mode string) {
	mode = strings.TrimSpace(mode)
	if isWebImagineQuotaMode(mode) {
		mode = accountdomain.QuotaGroupWebImagine
	}
	if id == 0 || (!isConsoleUsageQuotaMode(mode) && mode != "weekly" && mode != accountdomain.QuotaGroupWebImagine && !isWebChatQuotaMode(mode)) {
		return
	}
	key := strconv.FormatUint(id, 10) + ":" + mode
	s.quotaRefresh.mu.Lock()
	state := s.quotaRefresh.obs[key]
	now := s.now().UTC()
	if state != nil && !state.pending && !state.queued && !state.running && !now.Before(state.nextAttemptAt) {
		delete(s.quotaRefresh.obs, key)
		state = nil
	}
	if state == nil {
		state = &quotaRefreshState{}
		s.quotaRefresh.obs[key] = state
	}
	// 显式入队代表新的刷新需求（429 核实 / 迁移任务 / 巡检扫描）：失败
	// 计数归零、开启全新重试 episode，避免历史失败把新需求立即推进熔断停靠。
	state.failures = 0
	state.parkedUntil = time.Time{}
	state.generation++
	state.pending = true
	enqueued := state.queued || state.running || now.Before(state.nextAttemptAt) || s.enqueueQuotaRefreshLocked(quotaRefreshRequest{key: key, accountID: id, mode: mode}, state)
	s.quotaRefresh.mu.Unlock()
	if !enqueued {
		perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "enqueue", Outcome: "queue_full"}, 1)
		s.logger.Warn("quota_refresh_queue_full", "account_id", id, "mode", mode)
		s.wakeQuotaRefreshRecovery()
	}
}

func (s *Service) enqueueQuotaRefreshLocked(request quotaRefreshRequest, state *quotaRefreshState) bool {
	if state == nil || !state.ready(s.now().UTC()) {
		return state != nil
	}
	select {
	case s.quotaRefresh.queue <- request:
		state.queued = true
		return true
	default:
		return false
	}
}

func (s *Service) wakeQuotaRefreshRecovery() {
	select {
	case s.quotaRefresh.wake <- struct{}{}:
	default:
	}
}

// RunQuotaRefresh uses a fixed worker set to avoid unbounded goroutine creation.
func (s *Service) RunQuotaRefresh(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(managedTaskWorkerCeiling + 1)
	for range managedTaskWorkerCeiling {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case request := <-s.quotaRefresh.queue:
					s.quotaRefresh.mu.Lock()
					state := s.quotaRefresh.obs[request.key]
					if state == nil || !state.queued {
						s.quotaRefresh.mu.Unlock()
						continue
					}
					state.queued = false
					if !state.ready(s.now().UTC()) {
						s.quotaRefresh.mu.Unlock()
						continue
					}
					state.runningGeneration = state.generation
					state.runningVersion = state.sharedVersion
					state.running = true
					state.pending = false
					s.quotaRefresh.mu.Unlock()
					if err := batch.Do(ctx, func(workCtx context.Context) error {
						s.runQuotaRefresh(workCtx, request)
						return nil
					}); err != nil {
						s.deferQuotaRefresh(request.key)
						if ctx.Err() == nil {
							var panicErr *batch.PanicError
							if errors.As(err, &panicErr) {
								s.logger.Error("quota_refresh_worker_panicked", "account_id", request.accountID, "mode", request.mode, "error", panicErr, "stack", string(panicErr.Stack))
							} else {
								s.logger.Error("quota_refresh_worker_failed", "account_id", request.accountID, "mode", request.mode, "error", err)
							}
						}
					}
				}
			}
		}()
	}
	go func() {
		defer workers.Done()
		s.runQuotaRefreshRecovery(ctx)
	}()
	workers.Wait()
}

func (s *Service) runQuotaRefresh(parent context.Context, request quotaRefreshRequest) {
	for {
		s.quotaRefresh.mu.Lock()
		state := s.quotaRefresh.obs[request.key]
		if state == nil {
			s.quotaRefresh.mu.Unlock()
			return
		}
		if state.failures >= quotaRefreshFailureBudget {
			state.running = false
			s.quotaRefresh.mu.Unlock()
			s.wakeQuotaRefreshRecovery()
			return
		}
		localGeneration := state.generation
		publishedGeneration := state.publishedGeneration
		sharedVersion := state.sharedVersion
		state.runningGeneration = localGeneration
		state.runningVersion = sharedVersion
		state.pending = false
		s.quotaRefresh.mu.Unlock()

		ctx, cancel := context.WithTimeout(parent, quotaRefreshTimeout)
		if s.quotaRefreshState != nil && publishedGeneration < localGeneration {
			version, err := s.quotaRefreshState.MarkQuotaRefreshDirty(ctx, request.accountID, request.mode, quotaRefreshDirtyTTL)
			if err != nil {
				cancel()
				s.deferQuotaRefresh(request.key)
				perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "publish", Outcome: "failed"}, 1)
				s.logger.Warn("quota_refresh_dirty_publish_failed", "account_id", request.accountID, "mode", request.mode, "error", err)
				return
			}
			sharedVersion = version
			s.quotaRefresh.mu.Lock()
			if current := s.quotaRefresh.obs[request.key]; current != nil && current.publishedGeneration < localGeneration {
				current.publishedGeneration = localGeneration
				// Publishing our existing demand is not a fresh budget. A shared
				// scan may also have observed a newer publication before this reply.
				if current.canAdopt(version, s.now().UTC()) {
					current.sharedVersion = version
				}
				current.runningVersion = version
			}
			s.quotaRefresh.mu.Unlock()
		}
		if s.quotaRefreshState != nil && publishedGeneration >= localGeneration && sharedVersion.Generation > 0 {
			version, dirty, err := s.quotaRefreshState.GetQuotaRefreshState(ctx, request.accountID, request.mode)
			if err != nil {
				cancel()
				s.deferQuotaRefresh(request.key)
				return
			}
			if !version.Equal(sharedVersion) {
				sharedVersion = version
				s.quotaRefresh.mu.Lock()
				if current := s.quotaRefresh.obs[request.key]; current != nil {
					current.observe(version, s.now().UTC())
					current.runningVersion = version
				}
				s.quotaRefresh.mu.Unlock()
			}
			if !dirty {
				cancel()
				s.quotaRefresh.mu.Lock()
				if current := s.quotaRefresh.obs[request.key]; current != nil {
					newDemand := current.generation != localGeneration ||
						(!current.sharedVersion.Equal(sharedVersion) && s.now().UTC().Before(current.sharedVersion.ExpiresAt))
					switch {
					case newDemand:
						current.running, current.pending = false, true
					case current.durable && version.Generation == 0:
						// An expired signal says nothing about unresolved SQL. Re-publish
						// the same local demand without resetting its failure budget.
						current.publishedGeneration = 0
						current.running, current.pending = false, true
					default:
						delete(s.quotaRefresh.obs, request.key)
					}
				}
				s.quotaRefresh.mu.Unlock()
				s.wakeQuotaRefreshRecovery()
				return
			}
		}
		refreshMode := request.mode
		consoleMode := isConsoleUsageQuotaMode(request.mode)
		skipUpstream := false
		if windows, err := s.accounts.GetQuotaWindows(ctx, []uint64{request.accountID}); err == nil {
			if consoleMode {
				for _, window := range windows[request.accountID] {
					if window.Mode == request.mode && window.SyncedAt != nil && s.now().UTC().Sub(window.SyncedAt.UTC()) < consoleQuotaRefreshMinInterval {
						skipUpstream = true
						break
					}
				}
			} else if request.mode != accountdomain.QuotaGroupWebImagine {
				// Weekly remains a Grok Web capability. Console never inherits this
				// legacy mode and always refreshes its authoritative /usage snapshot.
				// Imagine 配额组走 /rest/media/imagine/quota_info，不可被改刷 weekly。
				for _, window := range windows[request.accountID] {
					if window.Mode == "weekly" {
						refreshMode = "weekly"
						break
					}
				}
			}
		}
		var refreshErr error
		acquired := true
		var release func()
		if !skipUpstream && s.refreshLock != nil {
			lockKey := "quota-refresh:" + strconv.FormatUint(request.accountID, 10) + ":" + refreshMode
			if consoleMode {
				// Every Console mode reads the same /usage snapshot. Serialize all
				// three kinds across instances to avoid duplicate upstream probes.
				lockKey = consoleQuotaRefreshLockKey(request.accountID)
			}
			release, acquired, refreshErr = s.refreshLock.Acquire(ctx, lockKey, quotaRefreshTimeout)
		}
		if !skipUpstream && refreshErr == nil && acquired {
			if err := s.syncPool.Do(ctx, func(workCtx context.Context) error {
				if refreshMode == accountdomain.QuotaGroupWebImagine {
					var refreshed quotaRefreshResult
					refreshed, refreshErr = s.refreshQuotaGroup(workCtx, request.accountID, refreshMode)
					if refreshErr == nil {
						refreshErr = s.reconcileQuotaGroupWindows(workCtx, refreshed.Credential.Provider, request.accountID, refreshed.Modes, refreshed.Windows)
					}
				} else {
					_, refreshErr = s.RefreshQuotaMode(workCtx, request.accountID, refreshMode)
				}
				return refreshErr
			}); err != nil {
				refreshErr = err
			}
		}
		if release != nil {
			release()
		}
		cancel()
		if refreshErr != nil || !acquired {
			if refreshErr != nil && !errors.Is(refreshErr, context.Canceled) {
				s.logger.Warn("quota_refresh_failed", "account_id", request.accountID, "mode", refreshMode, "error", refreshErr)
			}
			s.deferQuotaRefresh(request.key)
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "retry"}, 1)
			return
		}

		currentShared := sharedVersion
		sharedDirty := s.quotaRefreshState != nil
		if s.quotaRefreshState != nil {
			generationCtx, generationCancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			var generationErr error
			currentShared, sharedDirty, generationErr = s.quotaRefreshState.GetQuotaRefreshState(generationCtx, request.accountID, request.mode)
			generationCancel()
			if generationErr != nil {
				s.deferQuotaRefresh(request.key)
				return
			}
		}
		s.quotaRefresh.mu.Lock()
		state = s.quotaRefresh.obs[request.key]
		localChanged := state != nil && state.generation != localGeneration
		s.quotaRefresh.mu.Unlock()
		if localChanged || (s.quotaRefreshState != nil && !currentShared.Equal(sharedVersion)) {
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "trailing"}, 1)
			if consoleMode {
				s.deferSuccessfulQuotaRefresh(request.key, true)
				return
			}
			continue
		}
		if s.quotaRefreshState != nil && sharedDirty {
			clearCtx, clearCancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			cleared, clearErr := s.quotaRefreshState.ClearQuotaRefreshDirty(clearCtx, request.accountID, request.mode, sharedVersion)
			clearCancel()
			if clearErr != nil || !cleared {
				if clearErr != nil {
					s.logger.Warn("quota_refresh_dirty_clear_failed", "account_id", request.accountID, "mode", request.mode, "error", clearErr)
				}
				// Completion failures are retries too. A failed CAS or transport
				// must not spin inside one worker and query upstream without limit.
				s.deferQuotaRefresh(request.key)
				return
			}
		}
		s.quotaRefresh.mu.Lock()
		state = s.quotaRefresh.obs[request.key]
		if state != nil && state.generation == localGeneration && state.sharedVersion.Equal(sharedVersion) {
			if consoleMode {
				state.running = false
				state.pending = false
				state.failures = 0
				state.nextAttemptAt = s.now().UTC().Add(consoleQuotaRefreshMinInterval)
			} else {
				delete(s.quotaRefresh.obs, request.key)
			}
			s.quotaRefresh.mu.Unlock()
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "success"}, 1)
			return
		}
		if consoleMode && state != nil {
			state.running = false
			state.pending = true
			state.failures = 0
			state.nextAttemptAt = s.now().UTC().Add(consoleQuotaRefreshMinInterval)
			s.quotaRefresh.mu.Unlock()
			s.wakeQuotaRefreshRecovery()
			return
		}
		s.quotaRefresh.mu.Unlock()
	}
}

func (s *Service) deferQuotaRefresh(key string) {
	s.quotaRefresh.mu.Lock()
	if state := s.quotaRefresh.obs[key]; state != nil {
		state.running = false
		state.pending = true
		if state.generation == state.runningGeneration && state.sharedVersion.Equal(state.runningVersion) {
			state.failures++
			state.nextAttemptAt = s.now().UTC().Add(quotaRefreshRetryDelay(state.failures))
		}
	}
	s.quotaRefresh.mu.Unlock()
	s.wakeQuotaRefreshRecovery()
}

func (s *Service) deferSuccessfulQuotaRefresh(key string, pending bool) {
	s.quotaRefresh.mu.Lock()
	if state := s.quotaRefresh.obs[key]; state != nil {
		state.running = false
		state.pending = pending
		state.failures = 0
		state.nextAttemptAt = s.now().UTC().Add(consoleQuotaRefreshMinInterval)
	}
	s.quotaRefresh.mu.Unlock()
	s.wakeQuotaRefreshRecovery()
}

func quotaRefreshRetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	// 11 档让指数曲线在 ~12 次失败后自然逼近 30 分钟上限（1s<<11≈34min，
	// 由 max 截断）；预算内（8 次）爬到 ~2 分钟，余量留给未来预算上调。
	shift := min(failures-1, 11)
	delay := quotaRefreshBackoffBase * time.Duration(1<<shift)
	if delay > quotaRefreshBackoffMax {
		delay = quotaRefreshBackoffMax
	}
	// Equal jitter keeps retries bounded away from zero while preventing a
	// shared upstream outage from synchronizing every account worker.
	half := delay / 2
	if half <= 0 {
		return delay
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func (s *Service) runQuotaRefreshRecovery(ctx context.Context) {
	retryTicker := time.NewTicker(quotaRefreshPollInterval)
	sharedTicker := time.NewTicker(quotaRefreshSharedPoll)
	durableTicker := time.NewTicker(30 * time.Second)
	durableCursor := s.recoverDurableQuotaRefreshes(ctx, 0)
	defer retryTicker.Stop()
	defer sharedTicker.Stop()
	defer durableTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.quotaRefresh.wake:
			s.requeueQuotaRefreshes()
		case <-retryTicker.C:
			s.requeueQuotaRefreshes()
		case <-sharedTicker.C:
			s.recoverSharedQuotaRefreshes(ctx, s.now().UTC())
			s.requeueQuotaRefreshes()
		case <-durableTicker.C:
			durableCursor = s.recoverDurableQuotaRefreshes(ctx, durableCursor)
			s.requeueQuotaRefreshes()
		}
	}
}

func (s *Service) requeueQuotaRefreshes() {
	now := s.now().UTC()
	var parked []string
	s.quotaRefresh.mu.Lock()
	for key, state := range s.quotaRefresh.obs {
		if state == nil {
			delete(s.quotaRefresh.obs, key)
			continue
		}
		if state.failures >= quotaRefreshFailureBudget && !state.queued && !state.running {
			if state.pending {
				state.pending = false
				state.parkedUntil = state.sharedVersion.ExpiresAt
				if state.parkedUntil.IsZero() {
					state.parkedUntil = now.Add(quotaRefreshDirtyTTL)
				}
				parked = append(parked, key)
			}
			if !state.parkedUntil.IsZero() && !now.Before(state.parkedUntil) {
				if state.durable {
					state.sharedVersion = repository.QuotaRefreshVersion{}
					state.parkedUntil = time.Time{}
				} else {
					delete(s.quotaRefresh.obs, key)
				}
			}
			continue
		}
		if !state.pending {
			if !state.queued && !state.running && !now.Before(state.nextAttemptAt) {
				delete(s.quotaRefresh.obs, key)
			}
			continue
		}
		if !state.ready(now) {
			continue
		}
		separator := strings.IndexByte(key, ':')
		if separator <= 0 || separator == len(key)-1 {
			continue
		}
		accountID, err := strconv.ParseUint(key[:separator], 10, 64)
		if err != nil {
			continue
		}
		if !s.enqueueQuotaRefreshLocked(quotaRefreshRequest{key: key, accountID: accountID, mode: key[separator+1:]}, state) {
			break
		}
	}
	s.quotaRefresh.mu.Unlock()
	for _, key := range parked {
		accountID, mode := uint64(0), key
		if separator := strings.IndexByte(key, ':'); separator > 0 && separator < len(key)-1 {
			if parsed, err := strconv.ParseUint(key[:separator], 10, 64); err == nil {
				accountID, mode = parsed, key[separator+1:]
			}
		}
		perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "parked"}, 1)
		s.logger.Warn("quota_refresh_retry_parked", "account_id", accountID, "mode", mode, "failures", quotaRefreshFailureBudget, "hint", "连续配额同步失败已达熔断预算，自动重试已停止；请检查该账号凭据/出口可达性，或手动触发刷新开启新轮次")
	}
}

func (s *Service) recoverSharedQuotaRefreshes(parent context.Context, now time.Time) {
	if s.quotaRefreshState == nil {
		return
	}
	s.quotaRefresh.mu.Lock()
	cursor := s.quotaRefreshCursor
	s.quotaRefresh.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	values, next, err := s.quotaRefreshState.ScanQuotaRefreshDirty(ctx, now, cursor, 100)
	cancel()
	if err != nil {
		s.logger.Warn("quota_refresh_dirty_list_failed", "error", err)
		return
	}
	s.quotaRefresh.mu.Lock()
	defer s.quotaRefresh.mu.Unlock()
	s.quotaRefreshCursor = next
	for _, value := range values {
		key := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
		state := s.quotaRefresh.obs[key]
		if state == nil {
			state = &quotaRefreshState{generation: 1, publishedGeneration: 1}
			s.quotaRefresh.obs[key] = state
		}
		state.observe(value.Version, now)
		// Consume the whole bounded page even if the queue fills. Each demand
		// is retained locally and the next scan can progress past parked rows.
		s.enqueueQuotaRefreshLocked(quotaRefreshRequest{key: key, accountID: value.AccountID, mode: value.Mode}, state)
	}
}
