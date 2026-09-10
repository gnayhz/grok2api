package account

import (
	"context"
	"errors"
	"strconv"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// ConsumeQuota accepts a confirmed generation fact. A successful return is a
// durable handoff, including a pending refresh whose recovery this owner runs.
// No caller may apply its own second quota policy or cache subtraction.
func (s *Service) ConsumeQuota(ctx context.Context, value accountdomain.QuotaConsumption) (accountdomain.QuotaConsumptionReceipt, error) {
	receipt, err := s.accounts.ConsumeQuota(ctx, value, s.now().UTC())
	if err != nil {
		return receipt, err
	}
	if receipt.State == accountdomain.QuotaConsumptionPendingRefresh {
		s.QueueQuotaRefresh(value.AccountID, value.Mode)
	}
	return receipt, nil
}

func (s *Service) saveQuotaSnapshot(ctx context.Context, providerValue accountdomain.Provider, value repository.QuotaSnapshotWrite) error {
	// The snapshot is owned by this refresh. Apply account timing policy before
	// both persistence and the returned windows are shared with waiting callers.
	observedAt := value.SyncedAt
	if observedAt.IsZero() {
		observedAt = s.now().UTC()
	}
	for index, window := range value.Windows {
		value.Windows[index] = quotaWindowTiming(providerValue, window, observedAt)
	}

	if err := s.accounts.SaveQuotaSnapshot(ctx, value); err != nil {
		return err
	}
	for index := range value.Windows {
		value.Windows[index].SnapshotVersion = value.Revision + 1
		value.Windows[index].Revision = value.Revision + 1
	}
	return nil
}

// recoverDurableQuotaRefreshes shares the existing bounded worker/queue. It
// does not reset a running retry episode or the failure budget on every scan.
// A parked durable demand stays parked until an explicit new demand or restart.
func (s *Service) recoverDurableQuotaRefreshes(parent context.Context, afterID uint64) uint64 {
	s.quotaRefreshMu.Lock()
	if afterID == 0 {
		s.quotaDurableScan++
	}
	scan := s.quotaDurableScan
	s.quotaRefreshMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	values, err := s.accounts.ListPendingQuotaRefreshes(ctx, afterID, 100)
	if err != nil {
		s.logger.Warn("quota_consumption_recovery_list_failed", "error", err)
		return afterID
	}
	if len(values) == 0 {
		// Only a completed SQL pass can retire absent durable demand. Preserve
		// an unexpired shared tombstone so retiring SQL does not restart it.
		s.quotaRefreshMu.Lock()
		for key, state := range s.quotaRefreshes {
			if state == nil || !state.durable || state.durableSeenScan == scan || state.running || state.queued || state.failures < quotaRefreshFailureBudget {
				continue
			}
			state.durable = false
			if state.sharedVersion.Generation == 0 && (state.parkedUntil.IsZero() || !s.now().UTC().Before(state.parkedUntil)) {
				delete(s.quotaRefreshes, key)
			}
		}
		s.quotaRefreshMu.Unlock()
		return 0
	}
	now := s.now().UTC()
	var checkedID uint64
	for _, value := range values {
		afterID = max(afterID, value.AccountID)
		mode := value.Mode
		if isWebImagineQuotaMode(mode) {
			mode = accountdomain.QuotaGroupWebImagine
		}
		key := strconv.FormatUint(value.AccountID, 10) + ":" + mode
		s.quotaRefreshMu.Lock()
		if state := s.quotaRefreshes[key]; state != nil {
			state.durableSeenScan = scan
		}
		s.quotaRefreshMu.Unlock()
		if checkedID != value.AccountID {
			checkedID = value.AccountID
			_, err = s.accounts.GetQuotaRevision(ctx, value.AccountID)
			if errors.Is(err, repository.ErrNotFound) {
				if resolveErr := s.accounts.ResolveDeletedQuotaConsumptions(ctx, value.AccountID, now); resolveErr != nil {
					s.logger.Warn("quota_consumption_deleted_account_failed", "account_id", value.AccountID, "error", resolveErr)
				}
			}
		}
		if err != nil {
			continue
		}
		s.quotaRefreshMu.Lock()
		state := s.quotaRefreshes[key]
		if state == nil {
			state = &quotaRefreshState{generation: 1, pending: true}
			s.quotaRefreshes[key] = state
		} else if !state.running && !state.queued && state.failures < quotaRefreshFailureBudget {
			state.pending = true
		}
		state.durable = true
		state.durableSeenScan = scan
		if state.ready(now) {
			s.enqueueQuotaRefreshLocked(quotaRefreshRequest{key: key, accountID: value.AccountID, mode: mode}, state)
		}
		s.quotaRefreshMu.Unlock()
	}
	return afterID
}
