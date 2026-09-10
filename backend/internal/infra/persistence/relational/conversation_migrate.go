package relational

import (
	"fmt"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// Migration runs under the session lock. It keeps opaque nodes and response IDs,
// checks full visible prefixes, and repairs only uniquely identified old roots.
// Existing in-flight requests retain their ticket and become migration candidates
// again on the next reservation, so an old completion is never lost.
func (j *ConversationJournal) migrateVisibleHistory(tx *gorm.DB, s *conversationSessionModel, p repository.JournalReserve) error {
	var nodes []conversationTurnModel
	if e := tx.Where("session = ? AND generation = ?", s.ID, s.Generation).Order("created_at, id").Find(&nodes).Error; e != nil {
		return e
	}
	byID := map[string]*conversationTurnModel{}
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	done := map[string]bool{}
	visiting := map[string]bool{}
	var visit func(*conversationTurnModel) error
	visit = func(n *conversationTurnModel) error {
		if done[n.ID] {
			return nil
		}
		if visiting[n.ID] {
			return fmt.Errorf("journal parent cycle")
		}
		visiting[n.ID] = true
		input, e := j.decrypt(n.EncryptedInput)
		if e != nil {
			return e
		}
		output, e := j.decrypt(n.EncryptedOutput)
		if e != nil {
			return e
		}
		hash := p.BaseHash
		count := 0
		if n.Parent != "" {
			parent := byID[n.Parent]
			if parent == nil {
				return historydomain.ErrHistoryMissing
			}
			if e = visit(parent); e != nil {
				return e
			}
			hash = parent.PrefixHash
			count = parent.TotalCount
		}
		prefix := map[string]int{}
		for _, raw := range input {
			h, reason, e := p.ItemHash(raw)
			if e != nil {
				return e
			}
			if reason {
				continue
			}
			hash = journalDigest(hash + h)
			count++
			prefix[hash] = count
		}
		if n.Parent == "" {
			var parent *conversationTurnModel
			ambiguous := false
			for _, candidate := range nodes {
				if !done[candidate.ID] || candidate.TotalCount <= 0 {
					continue
				}
				if prefix[candidate.PrefixHash] != candidate.TotalCount {
					continue
				}
				if parent == nil || candidate.TotalCount > parent.TotalCount {
					parent = byID[candidate.ID]
					ambiguous = false
				} else if candidate.TotalCount == parent.TotalCount {
					ambiguous = true
				}
			}
			if ambiguous {
				return historydomain.ErrHistoryAmbiguous
			}
			if parent != nil {
				// Retain the original full input for verification below, then store only
				// the suffix. Reasoning at the suffix boundary may duplicate parent output;
				// restoration's exact-cipher deduplication handles that without text guessing.
				pos := 0
				cut := 0
				for cut < len(input) && pos < parent.TotalCount {
					_, reason, e := p.ItemHash(input[cut])
					if e != nil {
						return e
					}
					if !reason {
						pos++
					}
					cut++
				}
				encrypted, e := j.encrypt(input[cut:])
				if e != nil {
					return e
				}
				s.Bytes += int64(len(encrypted) - len(n.EncryptedInput))
				n.EncryptedInput = encrypted
				n.Parent = parent.ID
			}
		}
		n.InputCount = count
		for _, raw := range output {
			h, reason, e := p.ItemHash(raw)
			if e != nil {
				return e
			}
			if reason {
				continue
			}
			hash = journalDigest(hash + h)
			count++
		}
		n.PrefixHash = hash
		n.TotalCount = count
		if e = tx.Save(n).Error; e != nil {
			return e
		}
		done[n.ID] = true
		delete(visiting, n.ID)
		return nil
	}
	for i := range nodes {
		if e := visit(&nodes[i]); e != nil {
			return e
		}
	}
	var active int64
	if e := tx.Model(&conversationRequestModel{}).Where("session = ?", s.ID).Count(&active).Error; e != nil {
		return e
	}
	if active == 0 {
		s.VisibleHashVersion = 1
	}
	return tx.Save(s).Error
}
