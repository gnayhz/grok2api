package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func isRecordNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// 案件与当事方存储(B3 数据层)。本文件只提供无语义的存取原语——
// 案件状态机与裁决规则属于 court;登记处只提供持久化存取原语。

// CaseRecord 是案件的一行。
type CaseRecord struct {
	ID           uint64
	Status       model.CaseStatus
	Verdict      model.Verdict
	EvidenceJSON string
	OpenedAt     time.Time
	ClosedAt     *time.Time
	UpdatedAt    time.Time
}

// CaseEvidence supplies the immutable opening evidence to legacy task recovery.
func (r *Registry) CaseEvidence(ctx context.Context, caseID uint64) (string, bool, error) {
	record, found, err := r.GetCase(ctx, caseID)
	return record.EvidenceJSON, found, err
}

// PartyRecord 是案件当事方的一行。
type PartyRecord struct {
	CaseID      uint64
	Kind        model.PartyKind
	AccountID   uint64
	NodeID      uint64
	Epoch       uint64
	Role        model.PartyRole
	Disposition model.PartyDisposition
	UpdatedAt   time.Time
}

// CreateCase 立案:写一行 investigating 案件,返回案件号。
// evidenceJSON 由调用方保证脱敏(I24:无 IP 明文/账号名/密钥)。
func (r *Registry) CreateCase(ctx context.Context, openedAt time.Time, evidenceJSON string) (uint64, error) {
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	row := qCaseModel{
		Status:       string(model.CaseInvestigating),
		EvidenceJSON: evidenceJSON,
		OpenedAt:     openedAt.UTC(),
		UpdatedAt:    openedAt.UTC(),
	}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return 0, err
	}
	return row.ID, nil
}

// ListRecentCases 列出最近案件(开案在前,结案在后;面板裁决流)。
func (r *Registry) ListRecentCases(ctx context.Context, limit int) ([]CaseRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []qCaseModel
	if err := r.db.WithContext(ctx).Order("CASE WHEN status = 'investigating' THEN 0 ELSE 1 END, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	records := make([]CaseRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, CaseRecord{
			ID: row.ID, Status: model.CaseStatus(row.Status), Verdict: model.Verdict(row.Verdict),
			EvidenceJSON: row.EvidenceJSON, OpenedAt: row.OpenedAt, ClosedAt: row.ClosedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return records, nil
}

// CloseCase 以裁决结案(状态与裁决词一致性由 court 保证)。
func (r *Registry) CloseCase(ctx context.Context, caseID uint64, status model.CaseStatus, verdict model.Verdict, closedAt time.Time) error {
	return r.CloseCaseWithEvidence(ctx, caseID, status, verdict, "", closedAt)
}

// CloseCaseWithEvidence 以裁决结案并落证据链摘要(I25:定罪留档;
// evidenceJSON 由 court 保证脱敏 I24)。
func (r *Registry) CloseCaseWithEvidence(ctx context.Context, caseID uint64, status model.CaseStatus, verdict model.Verdict, evidenceJSON string, closedAt time.Time) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error {
			return w.CloseCaseWithEvidence(ctx, caseID, status, verdict, evidenceJSON, closedAt)
		})
	}
	if closedAt.IsZero() {
		closedAt = time.Now().UTC()
	}
	updates := map[string]any{
		"status":     string(status),
		"verdict":    string(verdict),
		"closed_at":  closedAt.UTC(),
		"updated_at": closedAt.UTC(),
	}
	if evidenceJSON != "" {
		updates["evidence_json"] = evidenceJSON
	}
	if err := r.db.WithContext(ctx).Model(&qCaseModel{}).Where("id = ?", caseID).Updates(updates).Error; err != nil {
		return err
	}
	if err := recordIncidentClosures(r.db.WithContext(ctx), &caseID); err != nil {
		return err
	}
	// 结案即中止在飞取证(批9 事故:销案后探针没有归宿,面板永远
	// 显示"取证在飞")。案件先落库,探针回收失败单独上抛,漏网的由
	// CancelOrphanProbes 兜底收敛。
	_, err := r.CancelProbesForCase(ctx, caseID, "案件已结,取证中止")
	return err
}

// GetCase 读取案件。
func (r *Registry) GetCase(ctx context.Context, caseID uint64) (CaseRecord, bool, error) {
	var row qCaseModel
	if err := r.db.WithContext(ctx).Where("id = ?", caseID).First(&row).Error; err != nil {
		if isRecordNotFound(err) {
			return CaseRecord{}, false, nil
		}
		return CaseRecord{}, false, err
	}
	return CaseRecord{
		ID: row.ID, Status: model.CaseStatus(row.Status), Verdict: model.Verdict(row.Verdict),
		EvidenceJSON: row.EvidenceJSON, OpenedAt: row.OpenedAt, ClosedAt: row.ClosedAt, UpdatedAt: row.UpdatedAt,
	}, true, nil
}

// UpsertParty 登记或更新案件当事方(程序处置状态)。
func (r *Registry) UpsertParty(ctx context.Context, record PartyRecord) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.UpsertParty(ctx, record) })
	}
	now := time.Now().UTC()
	row := qCasePartyModel{
		CaseID: record.CaseID, Kind: string(record.Kind),
		AccountID: record.AccountID, NodeID: record.NodeID, Epoch: record.Epoch,
		Role: string(record.Role), Disposition: string(record.Disposition),
		UpdatedAt: now,
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "case_id"}, {Name: "kind"}, {Name: "account_id"}, {Name: "node_id"}, {Name: "epoch"},
		},
		DoUpdates: clause.Assignments(map[string]any{
			"role":        string(record.Role),
			"disposition": string(record.Disposition),
			"updated_at":  now,
		}),
	}).Create(&row).Error
}

// UpdatePartyDisposition 更新当事方程序处置。
func (r *Registry) UpdatePartyDisposition(ctx context.Context, caseID uint64, kind model.PartyKind, accountID, nodeID, epoch uint64, disposition model.PartyDisposition) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error {
			return w.UpdatePartyDisposition(ctx, caseID, kind, accountID, nodeID, epoch, disposition)
		})
	}
	res := r.db.WithContext(ctx).Model(&qCasePartyModel{}).
		Where("case_id = ? AND kind = ? AND account_id = ? AND node_id = ? AND epoch = ?",
			caseID, string(kind), accountID, nodeID, epoch).
		Updates(map[string]any{"disposition": string(disposition), "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: case=%d kind=%s account=%d node=%d epoch=%d", ErrPartyNotFound,
			caseID, kind, accountID, nodeID, epoch)
	}
	return nil
}

// ListOpenCases 列出调查中的案件(court 评估节拍消费)。
func (r *Registry) ListOpenCases(ctx context.Context) ([]CaseRecord, error) {
	var rows []qCaseModel
	if err := r.db.WithContext(ctx).Where("status = ?", string(model.CaseInvestigating)).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	records := make([]CaseRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, CaseRecord{
			ID: row.ID, Status: model.CaseStatus(row.Status), Verdict: model.Verdict(row.Verdict),
			EvidenceJSON: row.EvidenceJSON, OpenedAt: row.OpenedAt, ClosedAt: row.ClosedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return records, nil
}

// OpenCaseForIncident 返回同一账号、同一出口 epoch 的调查中案件号;0=无。
//
// 案件的证据基线是一次具体的 (account, node, epoch) 降智事件。账号在
// 另一个出口再次降智时必须创建新案件，不能把新出口塞进旧案件：旧案件
// 的探针基线和裁决对象已经固定，复用它会让一个出口的结论错误释放或
// 错误惩罚另一个出口。没有出口节点的直连事件退回按账号幂等。
func (r *Registry) OpenCaseForIncident(ctx context.Context, accountID uint64, exit model.EpochKey) (uint64, error) {
	if exit.NodeID == 0 {
		type row struct {
			CaseID uint64 `gorm:"column:case_id"`
		}
		var rows []row
		err := r.db.WithContext(ctx).Table("q_case_party AS defendant").
			Select("defendant.case_id AS case_id").
			Joins("JOIN q_case AS cases ON cases.id = defendant.case_id AND cases.status = ?", string(model.CaseInvestigating)).
			Where("defendant.kind = ? AND defendant.account_id = ? AND defendant.disposition IN (?, ?, ?)",
				string(model.PartyAccount), accountID,
				string(model.DispositionRemanded), string(model.DispositionSentenced), string(model.DispositionReleased)).
			Where("NOT EXISTS (SELECT 1 FROM q_case_party AS exit_party WHERE exit_party.case_id = defendant.case_id AND exit_party.kind = ?)",
				string(model.PartyExit)).
			Order("defendant.case_id DESC").Limit(1).Find(&rows).Error
		if err != nil {
			return 0, err
		}
		if len(rows) == 0 {
			return 0, nil
		}
		return rows[0].CaseID, nil
	}
	type row struct {
		CaseID uint64 `gorm:"column:case_id"`
	}
	var rows []row
	err := r.db.WithContext(ctx).Table("q_case_party AS defendant").
		Select("defendant.case_id AS case_id").
		Joins("JOIN q_case AS cases ON cases.id = defendant.case_id AND cases.status = ?", string(model.CaseInvestigating)).
		Joins("JOIN q_case_party AS exit_party ON exit_party.case_id = defendant.case_id").
		Where("defendant.kind = ? AND defendant.account_id = ? AND defendant.disposition IN (?, ?, ?)",
			string(model.PartyAccount), accountID,
			string(model.DispositionRemanded), string(model.DispositionSentenced), string(model.DispositionReleased)).
		Where("exit_party.kind = ? AND exit_party.node_id = ? AND exit_party.epoch = ? AND exit_party.disposition IN (?, ?)",
			string(model.PartyExit), exit.NodeID, exit.Epoch, string(model.DispositionRemanded), string(model.DispositionReleased)).
		Order("defendant.case_id DESC").Limit(1).Find(&rows).Error
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].CaseID, nil
}

// ListParties 列出案件全部当事方。
func (r *Registry) ListParties(ctx context.Context, caseID uint64) ([]PartyRecord, error) {
	var rows []qCasePartyModel
	if err := r.db.WithContext(ctx).Where("case_id = ?", caseID).Find(&rows).Error; err != nil {
		return nil, err
	}
	records := make([]PartyRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, PartyRecord{
			CaseID: row.CaseID, Kind: model.PartyKind(row.Kind),
			AccountID: row.AccountID, NodeID: row.NodeID, Epoch: row.Epoch,
			Role: model.PartyRole(row.Role), Disposition: model.PartyDisposition(row.Disposition),
			UpdatedAt: row.UpdatedAt,
		})
	}
	return records, nil
}
