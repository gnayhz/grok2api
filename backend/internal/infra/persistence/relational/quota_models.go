package relational

import "time"

// Separate from credential/profile writes so ordinary account updates cannot
// reset the quota CAS revision. All quota mutations serialize on this row.
type quotaStateModel struct {
	AccountID uint64        `gorm:"primaryKey"`
	Revision  uint64        `gorm:"not null;default:0;check:chk_account_quota_state_revision,revision >= 0"`
	Account   *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (quotaStateModel) TableName() string { return "account_quota_state" }

// Consumption identities intentionally survive account deletion and audit
// retention. They contain no credentials or generated content.
type quotaConsumptionModel struct {
	EventID         string    `gorm:"size:160;primaryKey;not null;check:chk_account_quota_consumptions_event,length(event_id) BETWEEN 1 AND 160"`
	AccountID       uint64    `gorm:"not null;index:idx_quota_consumption_pending,priority:2;check:chk_account_quota_consumptions_account,account_id > 0"`
	Mode            string    `gorm:"size:64;not null;check:chk_account_quota_consumptions_mode,length(trim(mode)) BETWEEN 1 AND 64"`
	SnapshotVersion uint64    `gorm:"not null;check:chk_account_quota_consumptions_version,snapshot_version >= 0"`
	Units           int       `gorm:"not null;check:chk_account_quota_consumptions_units,units > 0"`
	State           string    `gorm:"size:24;not null;index:idx_quota_consumption_pending,priority:1;check:chk_account_quota_consumptions_state,state IN ('applied','pending_refresh','refreshed','account_deleted')"`
	CreatedAt       time.Time `gorm:"not null"`
	UpdatedAt       time.Time `gorm:"not null"`
}

func (quotaConsumptionModel) TableName() string { return "account_quota_consumptions" }
