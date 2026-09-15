package relational

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"gorm.io/gorm"
)

type requestAuditGenerationModel struct {
	ID                        uint64             `gorm:"primaryKey;autoIncrement"`
	AuditID                   uint64             `gorm:"not null;uniqueIndex:uidx_generation_ordinal;check:chk_generation_audit,audit_id > 0"`
	CreatedAt                 time.Time          `gorm:"not null;index:idx_generation_account_time,priority:2"`
	Audit                     *requestAuditModel `gorm:"foreignKey:AuditID;references:ID;constraint:OnDelete:CASCADE"`
	PhysicalID                string             `gorm:"not null;size:128;uniqueIndex:uidx_generation_physical;check:chk_generation_physical,length(physical_id) BETWEEN 1 AND 128"`
	Ordinal                   uint64             `gorm:"not null;uniqueIndex:uidx_generation_ordinal;check:chk_generation_ordinal,ordinal > 0"`
	AccountID                 uint64             `gorm:"not null;index:idx_generation_account_time,priority:1;check:chk_generation_account,account_id > 0"`
	AccountName               string             `gorm:"not null;default:'';size:160;check:chk_generation_account_name,length(account_name) <= 160"`
	Model                     string             `gorm:"not null;default:'';size:255;check:chk_generation_model,length(model) <= 255"`
	Selected                  bool               `gorm:"not null;default:false"`
	Outcome                   string             `gorm:"not null;size:16;check:chk_generation_outcome,outcome IN ('completed','failed','unconfirmed')"`
	UsageSource               string             `gorm:"not null;size:16;check:chk_generation_source,usage_source IN ('upstream','estimated','none')"`
	InputTokens               int64              `gorm:"not null;default:0;check:chk_generation_counters,input_tokens >= 0 AND cached_input_tokens >= 0 AND cache_creation_tokens >= 0 AND output_tokens >= 0 AND reasoning_tokens >= 0 AND total_tokens >= 0 AND context_input_tokens >= 0 AND context_output_tokens >= 0 AND num_sources_used >= 0 AND num_server_side_tools_used >= 0 AND cost_in_usd_ticks >= 0 AND estimated_cost_in_usd_ticks >= 0"`
	CachedInputTokens         int64              `gorm:"not null;default:0"`
	CachedInputTokensReported *bool
	CacheCreationTokens       int64  `gorm:"not null;default:0"`
	OutputTokens              int64  `gorm:"not null;default:0"`
	ReasoningTokens           int64  `gorm:"not null;default:0"`
	TotalTokens               int64  `gorm:"not null;default:0"`
	ContextInputTokens        int64  `gorm:"not null;default:0"`
	ContextOutputTokens       int64  `gorm:"not null;default:0"`
	NumSourcesUsed            int64  `gorm:"not null;default:0"`
	NumServerSideToolsUsed    int64  `gorm:"not null;default:0"`
	CostInUSDTicks            int64  `gorm:"not null;default:0"`
	EstimatedCostInUSDTicks   int64  `gorm:"not null;default:0"`
	PricingModel              string `gorm:"not null;default:'';size:100;check:chk_generation_pricing_model,length(pricing_model) <= 100"`
	PricingVersion            string `gorm:"not null;default:'';size:20;check:chk_generation_pricing_version,length(pricing_version) <= 20"`
}

func (requestAuditGenerationModel) TableName() string { return "request_audit_generations" }

func prepareGenerationModels(value audit.Record) ([]requestAuditGenerationModel, error) {
	if err := audit.ValidateGenerationUsages(value.GenerationUsages); err != nil {
		return nil, err
	}
	rows := make([]requestAuditGenerationModel, 0, len(value.GenerationUsages))
	for _, v := range value.GenerationUsages {
		rows = append(rows, requestAuditGenerationModel{CreatedAt: value.CreatedAt,
			PhysicalID:                v.PhysicalID,
			Ordinal:                   v.Ordinal,
			AccountID:                 v.AccountID,
			AccountName:               v.AccountName,
			Model:                     v.Model,
			Selected:                  v.Selected,
			Outcome:                   v.Outcome,
			UsageSource:               string(v.UsageSource),
			InputTokens:               v.InputTokens,
			CachedInputTokens:         v.CachedInputTokens,
			CachedInputTokensReported: v.CachedInputTokensReported,
			CacheCreationTokens:       v.CacheCreationTokens,
			OutputTokens:              v.OutputTokens,
			ReasoningTokens:           v.ReasoningTokens,
			TotalTokens:               v.TotalTokens,
			ContextInputTokens:        v.ContextInputTokens,
			ContextOutputTokens:       v.ContextOutputTokens,
			NumSourcesUsed:            v.NumSourcesUsed,
			NumServerSideToolsUsed:    v.NumServerSideToolsUsed,
			CostInUSDTicks:            v.CostInUSDTicks,
			EstimatedCostInUSDTicks:   v.EstimatedCostInUSDTicks,
			PricingModel:              v.PricingModel,
			PricingVersion:            v.PricingVersion,
		})
	}
	return rows, nil
}

func (v requestAuditGenerationModel) toDomain() audit.GenerationUsage {
	return audit.GenerationUsage{
		PhysicalID:                v.PhysicalID,
		Ordinal:                   v.Ordinal,
		AccountID:                 v.AccountID,
		AccountName:               v.AccountName,
		Model:                     v.Model,
		Selected:                  v.Selected,
		Outcome:                   v.Outcome,
		UsageSource:               audit.UsageSource(v.UsageSource),
		InputTokens:               v.InputTokens,
		CachedInputTokens:         v.CachedInputTokens,
		CachedInputTokensReported: v.CachedInputTokensReported,
		CacheCreationTokens:       v.CacheCreationTokens,
		OutputTokens:              v.OutputTokens,
		ReasoningTokens:           v.ReasoningTokens,
		TotalTokens:               v.TotalTokens,
		ContextInputTokens:        v.ContextInputTokens,
		ContextOutputTokens:       v.ContextOutputTokens,
		NumSourcesUsed:            v.NumSourcesUsed,
		NumServerSideToolsUsed:    v.NumServerSideToolsUsed,
		CostInUSDTicks:            v.CostInUSDTicks,
		EstimatedCostInUSDTicks:   v.EstimatedCostInUSDTicks,
		PricingModel:              v.PricingModel,
		PricingVersion:            v.PricingVersion,
	}
}

func insertAuditGenerations(tx *gorm.DB, auditID uint64, values []requestAuditGenerationModel) error {
	if len(values) == 0 {
		return nil
	}
	rows := append([]requestAuditGenerationModel(nil), values...)
	for i := range rows {
		rows[i].AuditID = auditID
	}
	return tx.CreateInBatches(&rows, 20).Error
}
