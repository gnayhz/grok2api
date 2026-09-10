package relational

import (
	"context"
	"database/sql"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// Inspect reads immutable old turns in one snapshot. It does not reserve a
// writer, extend expiry, migrate a scope, populate the hot cache, or delete data.
// The history owner alone decides whether the client's input covers these facts.
func (j *ConversationJournal) Inspect(ctx context.Context, scopes []repository.JournalScope, now time.Time) (snapshots []repository.JournalSnapshot, err error) {
	now = now.UTC()
	ids := make([]string, 0, len(scopes))
	seen := make(map[string]bool)
	for _, scope := range scopes {
		if scope.Key == "" {
			continue
		}
		id := journalScopeID(scope)
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	state := responsebuffer.NewState(responsebuffer.FromContext(ctx), 256<<20)
	defer state.Close()
	err = j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(ids); start += 400 {
			var sessions []conversationSessionModel
			if e := tx.Where("id IN ? AND expires_at > ? AND merged_into = ?", ids[start:min(start+400, len(ids))], now, "").Find(&sessions).Error; e != nil {
				return e
			}
			for _, session := range sessions {
				var rows []conversationTurnModel
				if e := tx.Where("session = ? AND generation = ?", session.ID, session.Generation).Find(&rows).Error; e != nil {
					return e
				}
				snapshot := repository.JournalSnapshot{}
				for _, row := range rows {
					if e := state.Grow(4*(len(row.EncryptedInput)+len(row.EncryptedOutput)), 0); e != nil {
						return e
					}
					input, e := j.decrypt(row.EncryptedInput)
					if e != nil {
						return e
					}
					output, e := j.decrypt(row.EncryptedOutput)
					if e != nil {
						return e
					}
					snapshot.Turns = append(snapshot.Turns, repository.JournalTurn{ID: row.ID, Parent: row.Parent, ResponseID: row.ResponseID, PrefixHash: row.PrefixHash, InputCount: row.InputCount, TotalCount: row.TotalCount, Input: input, Output: output})
				}
				snapshots = append(snapshots, snapshot)
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return
}
