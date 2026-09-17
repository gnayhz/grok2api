package model

import (
	"context"
	"log/slog"
	"time"
)

const (
	accountCatalogRefreshTimeout  = 30 * time.Second
	accountCatalogRefreshCapacity = 128
)

type accountSyncRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// QueueAccountSync accepts a catalog-change hint without delaying inference.
// The account's in-flight refresh is shared. M05 owns it through final writes;
// Close cancels and joins it before M01 releases network and storage.
// Capacity covers active and waiting accounts, before creating a goroutine or
// timer. Rejected hints do not advance the Provider's catalog baseline, so a
// later response can request the refresh again.
func (s *Service) QueueAccountSync(accountID uint64) bool {
	if accountID == 0 {
		return false
	}
	s.syncRunMu.Lock()
	if s.syncClosing {
		s.syncRunMu.Unlock()
		return false
	}
	if _, active := s.accountSyncRuns[accountID]; active {
		s.syncRunMu.Unlock()
		return true
	}
	if len(s.accountSyncRuns) >= accountCatalogRefreshCapacity {
		s.syncRunMu.Unlock()
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountCatalogRefreshTimeout)
	run := accountSyncRun{cancel: cancel, done: make(chan struct{})}
	if s.accountSyncRuns == nil {
		s.accountSyncRuns = make(map[uint64]accountSyncRun)
	}
	s.accountSyncRuns[accountID] = run
	s.syncRunMu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.syncRunMu.Lock()
			delete(s.accountSyncRuns, accountID)
			close(run.done)
			s.syncRunMu.Unlock()
		}()
		var count int
		err := s.bulkPool.Do(ctx, func(workCtx context.Context) error {
			var err error
			count, err = s.SyncAccount(workCtx, accountID)
			return err
		})
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		if err != nil {
			logger.Warn("model_etag_refresh_failed", "account_id", accountID, "error", err)
			return
		}
		logger.Info("model_etag_refresh_completed", "account_id", accountID, "models", count)
	}()
	return true
}
