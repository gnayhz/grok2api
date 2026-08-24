package relational

import (
	"context"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AccountRiskVerdict is the persisted RSC verdict for one Web SSO identity.
// Build accounts inherit the verdict of their linked Web account.
type AccountRiskVerdict struct {
	AccountID  uint64
	Verdict    string // clean | denied | flagged | error
	BotFlagSrc int
	BotFlagDtl string
	RiskScore  float64
	HTTPStatus int
	Error      string
	Source     string
	CheckedAt  time.Time
}

// RiskRepository persists RSC verdicts for Web SSO identities.
type RiskRepository struct{ db *Database }

func NewRiskRepository(db *Database) *RiskRepository { return &RiskRepository{db: db} }

func (r *RiskRepository) GetRiskVerdict(ctx context.Context, accountID uint64) (AccountRiskVerdict, error) {
	var row accountRiskVerdictModel
	if err := r.db.db.WithContext(ctx).First(&row, "account_id = ?", accountID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AccountRiskVerdict{}, repository.ErrNotFound
		}
		return AccountRiskVerdict{}, mapError(err)
	}
	return AccountRiskVerdict(row), nil
}

func (r *RiskRepository) SaveRiskVerdict(ctx context.Context, verdict AccountRiskVerdict) error {
	row := accountRiskVerdictModel(verdict)
	if row.Verdict == "" {
		row.Verdict = "error"
	}
	// botFlagDetails/Error are upstream-controlled text: centrally redact
	// secret-like key=value pairs, then truncate (defense in depth).
	row.BotFlagDtl = truncateRSCDetail(rscRedactSecrets(row.BotFlagDtl), 512)
	row.Error = truncateRSCDetail(rscRedactSecrets(row.Error), 512)
	return r.db.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// DeleteRiskVerdict permanently removes one identity's verdict (operator
// manual clear). Missing rows are a no-op: the goal is that no verdict stays.
func (r *RiskRepository) DeleteRiskVerdict(ctx context.Context, accountID uint64) error {
	return r.db.db.WithContext(ctx).
		Where("account_id = ?", accountID).
		Delete(&accountRiskVerdictModel{}).Error
}

// ListRiskyVerdictAccountIDs returns every account holding a denied/flagged
// verdict, paged by riskyVerdictPageLimit. Startup reconciliation uses it to
// converge risk_status flags with the verdict table (the durable source of truth).
func (r *RiskRepository) ListRiskyVerdictAccountIDs(ctx context.Context) ([]uint64, error) {
	return r.listRiskyVerdictAccountIDs(ctx, 0)
}

// ListRiskyVerdictAccountIDsAfter pages beyond the given account id cursor.
func (r *RiskRepository) ListRiskyVerdictAccountIDsAfter(ctx context.Context, afterID uint64) ([]uint64, error) {
	return r.listRiskyVerdictAccountIDs(ctx, afterID)
}

// riskyVerdictPageLimit bounds one reconciliation page; the cursor loop in the
// risk service walks every page so pools beyond the limit still converge.
const riskyVerdictPageLimit = 10000

func (r *RiskRepository) listRiskyVerdictAccountIDs(ctx context.Context, afterID uint64) ([]uint64, error) {
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("account_risk_verdicts AS verdict").
		Joins("JOIN provider_accounts AS account ON account.id = verdict.account_id").
		Where("verdict.verdict IN ?", []string{"denied", "flagged"}).
		Where("verdict.account_id > ?", afterID).
		Order("verdict.account_id ASC").
		Limit(riskyVerdictPageLimit).
		Pluck("verdict.account_id", &ids).Error
	return ids, mapError(err)
}

// ListPatrolDue returns enabled Web accounts whose stored verdict is due for
// a patrol re-check: clean verdicts older than patrolInterval, or error
// verdicts older than errorRetryAfter. Risky verdicts never re-check.
func (r *RiskRepository) ListPatrolDue(ctx context.Context, provider account.Provider, patrolInterval, errorRetryAfter time.Time, limit int) ([]uint64, error) {
	if limit <= 0 {
		limit = 500
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Model(&accountModel{}).
		Where("provider = ? AND enabled = ?", provider, true).
		Where(
			"id NOT IN (SELECT account_id FROM account_risk_verdicts WHERE verdict IN ('denied','flagged') OR (verdict = 'clean' AND checked_at > ?) OR (verdict = 'error' AND checked_at > ?))",
			patrolInterval, errorRetryAfter,
		).
		Limit(limit).
		Pluck("id", &ids).Error
	return ids, mapError(err)
}
