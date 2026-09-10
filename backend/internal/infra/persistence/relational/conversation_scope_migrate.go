package relational

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// LegacyBuildAccountIDs includes retired accounts still referenced by audit
// records. Only numeric IDs are read; migration never needs their credentials.
func (j *ConversationJournal) LegacyBuildAccountIDs(ctx context.Context) ([]uint64, error) {
	var ids, audited []uint64
	if e := j.db.WithContext(ctx).Model(&accountModel{}).Where("provider = ?", account.ProviderBuild).Pluck("id", &ids).Error; e != nil {
		return nil, e
	}
	if e := j.db.WithContext(ctx).Model(&requestAuditModel{}).Where("provider = ? AND account_id > 0", account.ProviderBuild).Distinct("account_id").Pluck("account_id", &audited).Error; e != nil {
		return nil, e
	}
	seen := map[uint64]bool{}
	var result []uint64
	for _, id := range append(ids, audited...) {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

// Merge only explicitly supplied legacy identities, never search other tenants
// by message text. The transaction moves retained generations verbatim, then the
// existing full-prefix migration reconnects uniquely matching account boundaries.
func (j *ConversationJournal) migrateAccountScopes(tx *gorm.DB, s *conversationSessionModel, p repository.JournalReserve) error {
	var ids []string
	seen := map[string]bool{s.ID: true}
	for _, scope := range p.LegacyScopes {
		if scope.Model != p.Scope.Model || scope.Normalizer != p.Scope.Normalizer {
			return fmt.Errorf("invalid legacy history scope")
		}
		id := journalScopeID(scope)
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	// Filter in bounded batches before taking locks: most account scopes do not
	// exist for this conversation, even on installations with many accounts.
	var existing []string
	for start := 0; start < len(ids); start += 400 {
		var batch []string
		if e := tx.Model(&conversationSessionModel{}).Where("id IN ?", ids[start:min(start+400, len(ids))]).Pluck("id", &batch).Error; e != nil {
			return e
		}
		existing = append(existing, batch...)
	}
	sort.Strings(existing)
	for _, id := range existing {
		old, e := lockJournal(tx, id)
		if errors.Is(e, historydomain.ErrHistoryStale) && old.MergedInto == s.ID {
			continue
		}
		if e != nil {
			return e
		}
		var active int64
		if e = tx.Model(&conversationRequestModel{}).Where("session = ? AND expires_at > ?", id, p.Now).Count(&active).Error; e != nil {
			return e
		}
		// An old process may still own a ticket. Retry after draining it; never
		// silently start a truncated conversation or invalidate its accepted output.
		if active > 0 {
			return fmt.Errorf("%w: legacy_history_in_flight", historydomain.ErrHistoryStale)
		}
		if old.ExpiresAt.After(p.Now) {
			var size int64
			if e = tx.Model(&conversationTurnModel{}).Where("session = ? AND generation = ?", id, old.Generation).Select("COALESCE(SUM(LENGTH(encrypted_input) + LENGTH(encrypted_output)), 0)").Scan(&size).Error; e != nil {
				return e
			}
			if s.Bytes+size > j.maxBytes {
				return historydomain.ErrHistoryQuota
			}
			if e = tx.Model(&conversationTurnModel{}).Where("session = ? AND generation = ?", id, old.Generation).Updates(map[string]any{"session": s.ID, "generation": s.Generation}).Error; e != nil {
				return e
			}
			s.Bytes += size
		}
		if e = deleteJournalData(tx, id); e != nil {
			return e
		}
		old.Head = ""
		old.Bytes = 0
		old.Generation++
		old.MergedInto = s.ID
		if e = tx.Save(&old).Error; e != nil {
			return e
		}
	}
	if s.Head == "" {
		var heads []string
		if e := tx.Model(&conversationTurnModel{}).Where("session = ? AND generation = ?", s.ID, s.Generation).Order("created_at DESC, id DESC").Limit(1).Pluck("id", &heads).Error; e != nil {
			return e
		}
		if len(heads) > 0 {
			s.Head = heads[0]
			s.Version++
		}
	}
	s.ScopeMigrated = true
	s.VisibleHashVersion = 0
	return tx.Save(s).Error
}
