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
	// OriginAccountID 触发判定的账号(通道隔离重放目标);0=旧数据。
	OriginAccountID uint64
	// Trigger 判定入口：degrade / patrol / manual。空=升级前旧行。
	Trigger string
	// DeniedStreak 连续 denied 次数（与 risk.StoredVerdict 逐字段镜像，
	// 二者靠直接结构转换互通）：达到 DeniedConfirmations 才处置。
	DeniedStreak int
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

// MostRecentCleanVerdict returns the account holding the newest clean verdict
// for the given probe source (the channel-vocabulary breaker witness), or
// found=false when none exists. maxAge>0 bounds how old the verdict may be:
// an ancient clean must not vouch for a channel vocabulary that may have
// changed since (aligned with the build-probe witness gate).
func (r *RiskRepository) MostRecentCleanVerdict(ctx context.Context, source string, maxAge time.Duration) (uint64, bool, error) {
	var row accountRiskVerdictModel
	query := r.db.db.WithContext(ctx).
		Where("verdict = ? AND source = ?", "clean", source)
	if maxAge > 0 {
		query = query.Where("checked_at > ?", time.Now().UTC().Add(-maxAge))
	}
	err := query.Order("checked_at DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, mapError(err)
	}
	return row.AccountID, true, nil
}

// DeleteCleanVerdictsExceptSources removes cached clean verdicts produced by
// a different probe method (a method switch invalidates the old cache
// wholesale: e.g. homepage-era cleans read every account as healthy once
// grok.com stopped delivering botFlag fields). denied/flagged rows are real
// detections and stay; error rows expire on their own retry window.
func (r *RiskRepository) DeleteCleanVerdictsExceptSources(ctx context.Context, keepSources ...string) (int64, error) {
	query := r.db.db.WithContext(ctx).Where("verdict = ?", "clean")
	if len(keepSources) > 0 {
		query = query.Where("source NOT IN ?", keepSources)
	}
	result := query.Delete(&accountRiskVerdictModel{})
	return result.RowsAffected, mapError(result.Error)
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
// a patrol re-check: clean verdicts older than patrolInterval, error
// verdicts older than errorRetryAfter, unconfirmed denials older than
// errorRetryAfter, and confirmed denied/flagged older than deniedTTLAfter.
// Accounts with no verdict row are not due — first detection is event-driven
// (OnDegraded). Including them turned a method switch that purged stale
// cleans into a full-pool scan that permanently flagged healthy identities.
//
// Unconfirmed denials (denied_streak < deniedConfirmations) become due after
// errorRetryAfter so the next patrol re-probes and either confirms (streak
// grows → consequences) or clears (clean overwrites). Confirmed denials
// become due after DeniedTTL so a clean re-read can unflag: rsc_denied
// excludes the selector, so OnDegraded/freshVerdict expiry alone can never
// self-heal a false conviction.
func (r *RiskRepository) ListPatrolDue(ctx context.Context, provider account.Provider, patrolInterval, errorRetryAfter, deniedTTLAfter time.Time, deniedConfirmations int, limit int) ([]uint64, error) {
	if limit <= 0 {
		limit = 500
	}
	if deniedConfirmations <= 0 {
		deniedConfirmations = 2
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("account_risk_verdicts AS verdict").
		Joins("JOIN provider_accounts AS account ON account.id = verdict.account_id").
		Where("account.provider = ? AND account.enabled = ?", provider, true).
		Where(
			"(verdict.verdict = 'clean' AND verdict.checked_at <= ?) OR (verdict.verdict = 'error' AND verdict.checked_at <= ?) OR (verdict.verdict = 'denied' AND verdict.denied_streak < ? AND verdict.checked_at <= ?) OR ((verdict.verdict = 'flagged' OR (verdict.verdict = 'denied' AND verdict.denied_streak >= ?)) AND verdict.checked_at <= ?)",
			patrolInterval, errorRetryAfter, deniedConfirmations, errorRetryAfter, deniedConfirmations, deniedTTLAfter,
		).
		Order("verdict.checked_at ASC").
		Limit(limit).
		Pluck("account.id", &ids).Error
	return ids, mapError(err)
}
