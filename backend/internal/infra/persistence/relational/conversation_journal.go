package relational

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type journalCipher interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

type conversationSessionModel struct {
	ID                 string    `gorm:"primaryKey;size:64"`
	Generation         int64     `gorm:"not null"`
	ScopeMigrated      bool      `gorm:"not null;default:false"`
	MergedInto         string    `gorm:"size:64;not null;default:''"`
	VisibleHashVersion int       `gorm:"not null;default:0"`
	Version            int64     `gorm:"not null"`
	Head               string    `gorm:"size:64;not null"`
	Bytes              int64     `gorm:"not null"`
	ExpiresAt          time.Time `gorm:"index;not null"`
}

func (conversationSessionModel) TableName() string { return "conversation_journal_sessions_v1" }

type conversationRequestModel struct {
	ID             string    `gorm:"primaryKey;size:64"`
	Session        string    `gorm:"index;size:64;not null"`
	Generation     int64     `gorm:"not null"`
	Version        int64     `gorm:"not null"`
	Parent         string    `gorm:"size:64;not null"`
	InputHash      string    `gorm:"size:64;not null"`
	InputCount     int       `gorm:"not null"`
	EncryptedInput string    `gorm:"type:text;not null"`
	Bytes          int64     `gorm:"not null"`
	ExpiresAt      time.Time `gorm:"index;not null"`
}

func (conversationRequestModel) TableName() string { return "conversation_journal_requests_v1" }

type conversationTurnModel struct {
	ID              string    `gorm:"primaryKey;size:64"`
	Session         string    `gorm:"index:idx_conversation_turn_prefix,priority:1;index:idx_conversation_turn_response,priority:1;size:64;not null"`
	Generation      int64     `gorm:"index:idx_conversation_turn_prefix,priority:2;index:idx_conversation_turn_response,priority:2;not null"`
	ResponseID      string    `gorm:"index:idx_conversation_turn_response,priority:3;size:256;not null"`
	Parent          string    `gorm:"size:64;not null"`
	PrefixHash      string    `gorm:"index:idx_conversation_turn_prefix,priority:3;size:64;not null"`
	InputCount      int       `gorm:"not null"`
	TotalCount      int       `gorm:"not null"`
	EncryptedInput  string    `gorm:"type:text;not null"`
	EncryptedOutput string    `gorm:"type:text;not null"`
	OutputHash      string    `gorm:"size:64;not null"`
	CreatedAt       time.Time `gorm:"not null"`
}

func (conversationTurnModel) TableName() string { return "conversation_journal_turns_v1" }

// ConversationJournal stores immutable encrypted per-turn deltas. All writers
// lock the session row before reserving, committing or resetting. SQLite uses
// IMMEDIATE transactions; PostgreSQL obtains the same lock with UPDATE.
// Quotas protect retained history: exceeding the limit rejects the new write.
type ConversationJournal struct {
	db       *gorm.DB
	cipher   journalCipher
	maxBytes int64
	hot      repository.ReasoningReplayRepository
	hotTTL   time.Duration
}

// WithHotCache accelerates immutable output decoding. Every hit is checked
// against the durable node's digest; cache errors and evictions are misses.
func (j *ConversationJournal) WithHotCache(hot repository.ReasoningReplayRepository, ttl time.Duration) *ConversationJournal {
	if ttl <= 0 {
		ttl = time.Hour
	}
	j.hot, j.hotTTL = hot, ttl
	return j
}

func (j *ConversationJournal) output(hotCtx context.Context, node conversationTurnModel) ([][]byte, error) {
	key := node.ID + ":" + node.OutputHash
	if j.hot != nil && hotCtx.Err() == nil {
		if items, ok, err := j.hot.Get(hotCtx, "journal-v1", key, time.Now(), j.hotTTL); err == nil && ok {
			raw, _ := json.Marshal(items)
			if journalDigest(string(raw)) == node.OutputHash {
				return items, nil
			}
		}
	}
	items, err := j.decrypt(node.EncryptedOutput)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(items)
	if journalDigest(string(raw)) != node.OutputHash {
		return nil, fmt.Errorf("journal output digest mismatch")
	}
	if j.hot != nil && hotCtx.Err() == nil {
		_ = j.hot.Set(hotCtx, "journal-v1", key, items, time.Now().Add(j.hotTTL))
	}
	return items, nil
}

func NewConversationJournal(db *Database, cipher journalCipher, maxBytes int64) *ConversationJournal {
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	return &ConversationJournal{db: db.db, cipher: cipher, maxBytes: maxBytes}
}
func journalDigest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func journalScopeID(s repository.JournalScope) string {
	return journalDigest(fmt.Sprintf("journal:v1:%d:%s:%s", s.Normalizer, s.Model, s.Key))
}
func journalRequestID(scope, token string) string { return journalDigest(scope + ":" + token) }
func (j *ConversationJournal) encrypt(items [][]byte) (string, error) {
	b, e := json.Marshal(items)
	if e != nil {
		return "", e
	}
	return j.cipher.Encrypt(string(b))
}
func (j *ConversationJournal) decrypt(v string) ([][]byte, error) {
	b, e := j.cipher.Decrypt(v)
	if e != nil {
		return nil, e
	}
	var items [][]byte
	e = json.Unmarshal([]byte(b), &items)
	return items, e
}
func lockJournal(tx *gorm.DB, id string) (conversationSessionModel, error) {
	var s conversationSessionModel
	r := tx.Model(&conversationSessionModel{}).Where("id = ?", id).UpdateColumn("version", gorm.Expr("version + 0"))
	if r.Error != nil {
		return s, r.Error
	}
	if r.RowsAffected == 0 {
		return s, historydomain.ErrHistoryMissing
	}
	e := tx.First(&s, "id = ?", id).Error
	if e == nil && s.MergedInto != "" {
		return s, historydomain.ErrHistoryStale
	}
	return s, e
}
func deleteJournalData(tx *gorm.DB, id string) error {
	if e := tx.Where("session = ?", id).Delete(&conversationRequestModel{}).Error; e != nil {
		return e
	}
	return tx.Where("session = ?", id).Delete(&conversationTurnModel{}).Error
}
func (j *ConversationJournal) Reserve(ctx context.Context, p repository.JournalReserve) (out repository.JournalReservation, err error) {
	if p.Token == "" || p.Scope.Key == "" || p.Scope.Model == "" || p.Scope.Normalizer <= 0 || p.Retention <= 0 || p.Lease <= 0 || len(p.Input) != len(p.Prefixes) {
		return out, fmt.Errorf("invalid journal reservation")
	}
	id := journalScopeID(p.Scope)
	rid := journalRequestID(id, p.Token)
	var selectedParent conversationTurnModel
	err = j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		initial := conversationSessionModel{ID: id, Generation: 1, ExpiresAt: p.Now.Add(p.Retention)}
		if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; e != nil {
			return e
		}
		s, e := lockJournal(tx, id)
		if e != nil {
			return e
		}
		// Abandoned attempts consume quota only for their lease. Perform this
		// accounting under the same session lock as all quota admissions.
		var expiredBytes int64
		if e = tx.Model(&conversationRequestModel{}).Where("session = ? AND expires_at <= ?", id, p.Now).Select("COALESCE(SUM(bytes), 0)").Scan(&expiredBytes).Error; e != nil {
			return e
		}
		if e = tx.Where("session = ? AND expires_at <= ?", id, p.Now).Delete(&conversationRequestModel{}).Error; e != nil {
			return e
		}
		s.Bytes -= expiredBytes
		if !s.ExpiresAt.After(p.Now) {
			var active int64
			if e = tx.Model(&conversationRequestModel{}).Where("session = ?", id).Count(&active).Error; e != nil {
				return e
			}
			if active == 0 {
				if e = deleteJournalData(tx, id); e != nil {
					return e
				}
				s.Generation++
				s.Version = 0
				s.Head = ""
				s.Bytes = 0
			}
		}
		if len(p.LegacyScopes) > 0 && !s.ScopeMigrated {
			if e = j.migrateAccountScopes(tx, &s, p); e != nil {
				return e
			}
		}
		if p.ItemHash != nil && s.VisibleHashVersion == 0 {
			if e = j.migrateVisibleHistory(tx, &s, p); e != nil {
				return e
			}
		}
		var prior conversationRequestModel
		e = tx.First(&prior, "id = ?", rid).Error
		if e == nil {
			return repository.ErrConflict
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		var parent conversationTurnModel
		if p.ParentResponseID != "" {
			var matches []conversationTurnModel
			if e = tx.Where("session = ? AND generation = ? AND response_id = ?", id, s.Generation, p.ParentResponseID).Limit(2).Find(&matches).Error; e != nil {
				return e
			}
			if len(matches) == 0 {
				return historydomain.ErrHistoryMissing
			}
			if len(matches) > 1 {
				return historydomain.ErrHistoryAmbiguous
			}
			parent = matches[0]
		} else {
			// Chunk IN clauses for SQLite's parameter bound. Equal visible history
			// with different producer outputs is ambiguous, even if one is the head.
			best := 0
			matches := 0
			for start := 0; start < len(p.Prefixes); start += 400 {
				end := min(start+400, len(p.Prefixes))
				var rows []conversationTurnModel
				if e = tx.Where("session = ? AND generation = ? AND prefix_hash IN ?", id, s.Generation, p.Prefixes[start:end]).Find(&rows).Error; e != nil {
					return e
				}
				for _, v := range rows {
					if v.TotalCount > best {
						best = v.TotalCount
						matches = 1
						parent = v
					} else if v.TotalCount == best {
						matches++
					}
				}
			}
			if matches > 1 {
				return historydomain.ErrHistoryAmbiguous
			}
		}
		out.Outcome = "new_history"
		suffixStart := 0
		inputCount := len(p.Input)
		inputHash := p.BaseHash
		if len(p.Prefixes) > 0 {
			inputHash = p.Prefixes[len(p.Prefixes)-1]
		}
		if parent.ID != "" {
			if p.Incremental {
				if len(p.ItemHashes) != len(p.Input) {
					return fmt.Errorf("invalid incremental history")
				}
				inputHash = parent.PrefixHash
				for _, itemHash := range p.ItemHashes {
					inputHash = journalDigest(inputHash + itemHash)
				}
				inputCount += parent.TotalCount
			} else if parent.TotalCount > len(p.Prefixes) || parent.TotalCount <= 0 || p.Prefixes[parent.TotalCount-1] != parent.PrefixHash {
				return historydomain.ErrHistoryMissing
			} else {
				suffixStart = parent.TotalCount
			}
			out.Outcome = "append_ok"
			selectedParent = parent
		} else if s.Head != "" {
			out.Outcome = "prefix_edited"
		}
		persistedInput := make([][]byte, 0, len(p.Input)-suffixStart)
		for i := suffixStart; i <= len(p.Input); i++ {
			persistedInput = append(persistedInput, p.InputReasoning[i]...)
			if i < len(p.Input) {
				persistedInput = append(persistedInput, p.Input[i])
			}
		}
		encrypted, e := j.encrypt(persistedInput)
		if e != nil {
			return e
		}
		size := int64(len(encrypted))
		if s.Bytes+size > j.maxBytes {
			return historydomain.ErrHistoryQuota
		}
		req := conversationRequestModel{ID: rid, Session: id, Generation: s.Generation, Version: s.Version, Parent: parent.ID, InputHash: inputHash, InputCount: inputCount, EncryptedInput: encrypted, Bytes: size, ExpiresAt: p.Now.Add(p.Lease)}
		if e = tx.Create(&req).Error; e != nil {
			return e
		}
		s.Bytes += size
		s.ExpiresAt = p.Now.Add(p.Retention)
		if e = tx.Save(&s).Error; e != nil {
			return e
		}
		out.Ticket = repository.JournalTicket{Scope: p.Scope, Token: p.Token, Parent: parent.ID, InputHash: inputHash, Generation: s.Generation, Version: s.Version, InputCount: inputCount}
		return nil
	})
	// The reservation protects the immutable chain from GC. Slow hot-cache
	// reads and decryption run after releasing the session write lock.
	if err == nil && selectedParent.ID != "" && !p.Incremental {
		out.Turns, err = j.loadChain(j.db.WithContext(ctx), id, out.Ticket.Generation, selectedParent)
		if err != nil {
			releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = j.Release(releaseCtx, out.Ticket)
		}
	}
	return
}
func (j *ConversationJournal) loadChain(tx *gorm.DB, session string, generation int64, node conversationTurnModel) ([]repository.JournalTurn, error) {
	hotCtx, cancel := context.WithTimeout(tx.Statement.Context, 50*time.Millisecond)
	defer cancel()
	state := responsebuffer.NewState(responsebuffer.FromContext(tx.Statement.Context), 256<<20)
	defer state.Close()
	var turns []repository.JournalTurn
	seen := map[string]bool{}
	for {
		if seen[node.ID] {
			return nil, fmt.Errorf("journal parent cycle")
		}
		seen[node.ID] = true
		if err := state.Grow(4*(len(node.EncryptedInput)+len(node.EncryptedOutput)), 0); err != nil {
			return nil, err
		}
		input, e := j.decrypt(node.EncryptedInput)
		if e != nil {
			return nil, e
		}
		output, e := j.output(hotCtx, node)
		if e != nil {
			return nil, e
		}
		turns = append(turns, repository.JournalTurn{ID: node.ID, ResponseID: node.ResponseID, Parent: node.Parent, PrefixHash: node.PrefixHash, InputCount: node.InputCount, TotalCount: node.TotalCount, Input: input, Output: output})
		if node.Parent == "" {
			break
		}
		var parent conversationTurnModel
		if e = tx.First(&parent, "id = ? AND session = ? AND generation = ?", node.Parent, session, generation).Error; e != nil {
			return nil, historydomain.ErrHistoryMissing
		}
		node = parent
	}
	for a, b := 0, len(turns)-1; a < b; a, b = a+1, b-1 {
		turns[a], turns[b] = turns[b], turns[a]
	}
	return turns, nil
}
func (j *ConversationJournal) Commit(ctx context.Context, p repository.JournalCommit) error {
	id := journalScopeID(p.Ticket.Scope)
	rid := journalRequestID(id, p.Ticket.Token)
	encoded, e := j.encrypt(p.Output)
	if e != nil {
		return e
	}
	raw, e := json.Marshal(p.Output)
	if e != nil {
		return e
	}
	outputHash := journalDigest(string(raw))
	return j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		s, e := lockJournal(tx, id)
		if e != nil {
			return e
		}
		if s.Generation != p.Ticket.Generation {
			return historydomain.ErrHistoryStale
		}
		var prior conversationTurnModel
		e = tx.First(&prior, "id = ?", rid).Error
		if e == nil {
			if prior.OutputHash == outputHash && prior.ResponseID == p.ResponseID && prior.PrefixHash == p.PrefixHash {
				return nil
			}
			return repository.ErrConflict
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		var req conversationRequestModel
		if e = tx.First(&req, "id = ? AND session = ?", rid, id).Error; e != nil {
			return historydomain.ErrHistoryMissing
		}
		if req.Generation != p.Ticket.Generation || req.Parent != p.Ticket.Parent || req.Version != p.Ticket.Version || req.InputHash != p.Ticket.InputHash || req.InputCount != p.Ticket.InputCount {
			return repository.ErrConflict
		}
		if !req.ExpiresAt.After(p.Now) {
			return historydomain.ErrHistoryStale
		}
		if p.TotalCount < req.InputCount || p.PrefixHash == "" {
			return fmt.Errorf("invalid journal checkpoint")
		}
		if s.Bytes+int64(len(encoded)) > j.maxBytes {
			return historydomain.ErrHistoryQuota
		}
		node := conversationTurnModel{ID: rid, Session: id, Generation: s.Generation, ResponseID: p.ResponseID, Parent: req.Parent, PrefixHash: p.PrefixHash, InputCount: req.InputCount, TotalCount: p.TotalCount, EncryptedInput: req.EncryptedInput, EncryptedOutput: encoded, OutputHash: outputHash, CreatedAt: p.Now}
		if e = tx.Create(&node).Error; e != nil {
			return e
		}
		if e = tx.Delete(&req).Error; e != nil {
			return e
		}
		// A sibling remains reachable by checkpoint but never replaces the head
		// of a branch that advanced since its reservation.
		if s.Version == req.Version && s.Head == req.Parent {
			s.Head = rid
			s.Version++
		}
		s.Bytes += int64(len(encoded))
		return tx.Save(&s).Error
	})
}
func (j *ConversationJournal) Reset(ctx context.Context, scope repository.JournalScope, now time.Time, expected ...int64) error {
	id := journalScopeID(scope)
	return j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		initial := conversationSessionModel{ID: id, Generation: 1, ExpiresAt: now}
		if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; e != nil {
			return e
		}
		s, e := lockJournal(tx, id)
		if e != nil {
			return e
		}
		if len(expected) > 0 && s.Generation != expected[0] {
			return historydomain.ErrHistoryStale
		}
		s.Generation++
		s.Head = ""
		s.Version = 0
		return tx.Save(&s).Error
	})
}
func (j *ConversationJournal) Prune(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []conversationSessionModel
	if e := j.db.WithContext(ctx).Where("expires_at <= ?", now).Order("expires_at").Limit(limit).Find(&rows).Error; e != nil {
		return 0, e
	}
	var count int64
	for _, row := range rows {
		e := j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			s, e := lockJournal(tx, row.ID)
			if errors.Is(e, historydomain.ErrHistoryStale) && s.MergedInto != "" {
				if err := tx.Delete(&s).Error; err != nil {
					return err
				}
				count++
				return nil
			}
			if errors.Is(e, historydomain.ErrHistoryMissing) {
				return nil
			}
			if e != nil {
				return e
			}
			if s.ExpiresAt.After(now) {
				return nil
			}
			var active int64
			if e = tx.Model(&conversationRequestModel{}).Where("session = ? AND expires_at > ?", s.ID, now).Count(&active).Error; e != nil {
				return e
			}
			if active > 0 {
				return nil
			}
			if e = deleteJournalData(tx, s.ID); e != nil {
				return e
			}
			if e = tx.Delete(&s).Error; e != nil {
				return e
			}
			count++
			return nil
		})
		if e != nil {
			return count, e
		}
	}
	return count, nil
}

// Release removes only an uncommitted reservation. Accepted immutable turns
// remain reachable when delivery is interrupted after successful commit.
func (j *ConversationJournal) Release(ctx context.Context, ticket repository.JournalTicket) error {
	id := journalScopeID(ticket.Scope)
	return j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		s, e := lockJournal(tx, id)
		if errors.Is(e, historydomain.ErrHistoryMissing) {
			return nil
		}
		if e != nil {
			return e
		}
		var req conversationRequestModel
		e = tx.First(&req, "id = ? AND session = ?", journalRequestID(id, ticket.Token), id).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		if e = tx.Delete(&req).Error; e != nil {
			return e
		}
		s.Bytes -= req.Bytes
		return tx.Save(&s).Error
	})
}
