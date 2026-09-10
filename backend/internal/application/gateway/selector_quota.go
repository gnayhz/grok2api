package gateway

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type quotaProjectionKey struct {
	accountID uint64
	mode      string
}

type quotaProjectionEntry struct {
	value     account.QuotaProjection
	expiresAt time.Time
}

const maxQuotaProjections = 100_000

func (s *Selector) applyQuotaInvalidation(event repository.InvalidationEvent) {
	now := time.Now().UTC()
	published := event.PublishedAt
	if published.IsZero() {
		published = now
	}
	expires := published.Add(candidateCacheStaleTTL + candidateCacheTTL)
	if !now.Before(expires) {
		return
	}
	key := quotaProjectionKey{event.AccountID, event.Quota.Mode}
	s.quotaProjectionMu.Lock()
	if s.quotaProjections == nil {
		s.quotaProjections = make(map[quotaProjectionKey]quotaProjectionEntry)
	}
	current, exists := s.quotaProjections[key]
	if exists && current.value.Revision >= event.Quota.Revision {
		s.quotaProjectionMu.Unlock()
		return
	}
	if !exists && len(s.quotaProjections) >= maxQuotaProjections {
		// Force a database reload instead of discarding a current reduction while
		// continuing to serve an old large-pool snapshot.
		clear(s.quotaProjections)
		s.quotaProjections[key] = quotaProjectionEntry{value: *event.Quota, expiresAt: expires}
		s.quotaProjectionMu.Unlock()
		s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged})
		return
	}
	s.quotaProjections[key] = quotaProjectionEntry{value: *event.Quota, expiresAt: expires}
	s.quotaProjectionMu.Unlock()
}

func (s *Selector) applyQuotaProjection(candidate account.RoutingCandidate) account.RoutingCandidate {
	if candidate.QuotaWindow == nil {
		return candidate
	}
	key := quotaProjectionKey{candidate.Credential.ID, candidate.QuotaWindow.Mode}
	s.quotaProjectionMu.RLock()
	entry, ok := s.quotaProjections[key]
	s.quotaProjectionMu.RUnlock()
	if !ok || entry.value.Revision <= candidate.QuotaWindow.Revision || entry.value.SnapshotVersion < candidate.QuotaWindow.SnapshotVersion {
		return candidate
	}
	if !time.Now().Before(entry.expiresAt) {
		s.quotaProjectionMu.Lock()
		if current, exists := s.quotaProjections[key]; exists && current.value.Revision == entry.value.Revision {
			delete(s.quotaProjections, key)
		}
		s.quotaProjectionMu.Unlock()
		return candidate
	}
	window := *candidate.QuotaWindow
	window.Remaining, window.SnapshotVersion, window.Revision = entry.value.Remaining, entry.value.SnapshotVersion, entry.value.Revision
	candidate.QuotaWindow = &window
	return candidate
}
