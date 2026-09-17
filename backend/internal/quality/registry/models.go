package registry

import (
	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"time"
)

// 九张 q_ 表(B3)是质量层自己的状态与数据表(D4):与底座表同库但不设
// 跨层外键——质量层可整体剥离(D2),对底座行的引用以裸 uint64 表达。
// 命名与列语义逐条对齐 REWRITE-BLUEPRINT.md B3 的表清单。

// qAccountStateModel 账号质量状态(B1.1)。稀疏表示:无行=ACTIVE——
// 未涉案账号不占行,未定罪即无痕(I12)由"释放即删行"物理保证。
type qAccountStateModel struct {
	AccountID  uint64    `gorm:"primaryKey"`
	State      string    `gorm:"size:16;not null;check:chk_q_account_state_state,state IN ('active','remanded','sentenced')"`
	StateSince time.Time `gorm:"not null"`
	// CurrentCaseID 当前关联案件号;0=无。羁押/服刑必须挂案件号。
	CurrentCaseID uint64    `gorm:"not null;default:0"`
	UpdatedAt     time.Time `gorm:"not null"`
}

func (qAccountStateModel) TableName() string { return "q_account_state" }

// qExitStateModel 出口质量状态(B1.2),按 (节点,IP-epoch) 键控(I15)。
// 稀疏表示:无行=AVAILABLE;释放即删行(调度无痕,台账留痕)。
type qExitStateModel struct {
	NodeID     uint64    `gorm:"primaryKey"`
	Epoch      uint64    `gorm:"primaryKey;default:0"`
	State      string    `gorm:"size:16;not null;check:chk_q_exit_state_state,state IN ('available','remanded','banned')"`
	StateSince time.Time `gorm:"not null"`
	// CurrentCaseID 当前关联案件号;0=无。
	CurrentCaseID uint64    `gorm:"not null;default:0"`
	UpdatedAt     time.Time `gorm:"not null"`
}

func (qExitStateModel) TableName() string { return "q_exit_state" }

// qIdentityGroupModel 身份组成员(B3 决议2):同 SSO 凭证身份的账号
// 集合(沿用 web_account_links 语义),用于排除同一身份的重复样本。
// 只登记多成员组;单账号缺省自成一组(查询缺席即自身)。
type qIdentityGroupModel struct {
	GroupID   uint64    `gorm:"primaryKey"`
	AccountID uint64    `gorm:"primaryKey;uniqueIndex:uidx_q_identity_group_account"`
	AddedAt   time.Time `gorm:"not null"`
}

func (qIdentityGroupModel) TableName() string { return "q_identity_group" }

// qCaseModel 案件(B1.3):单次降智事件链。证据链 JSON 只含规则指纹与
// 键控引用(节点+epoch/账号 ID),不含 IP 明文、账号名、密钥(I24)。
// verdict/status CHECK 中的 'dismissed' 仅兼容历史存量行,新写入路径不再产生该值。
type qCaseModel struct {
	ID           uint64    `gorm:"primaryKey;autoIncrement"`
	Status       string    `gorm:"size:32;not null;check:chk_q_case_status,status IN ('investigating','account_guilty','exit_guilty','dismissed')"`
	Verdict      string    `gorm:"size:32;not null;default:'';check:chk_q_case_verdict,verdict IN ('','account_guilty','exit_guilty','insufficient','dismissed')"`
	EvidenceJSON string    `gorm:"type:text;not null;default:'';check:chk_q_case_evidence,length(evidence_json) <= 65536"`
	OpenedAt     time.Time `gorm:"not null"`
	ClosedAt     *time.Time
	UpdatedAt    time.Time `gorm:"not null"`
}

func (qCaseModel) TableName() string { return "q_case" }

// qCasePartyModel 案件当事方(B1.3):账号/出口,角色与程序处置状态。
// disposition CHECK 中的 'dismissed' 仅兼容历史存量行,新写入路径不再产生该值。
type qCasePartyModel struct {
	ReviewReleased bool      `gorm:"not null;default:false"`
	ID             uint64    `gorm:"primaryKey;autoIncrement"`
	CaseID         uint64    `gorm:"not null;uniqueIndex:uidx_q_case_party_ref,priority:1;index:idx_q_case_party_case"`
	Kind           string    `gorm:"size:16;not null;uniqueIndex:uidx_q_case_party_ref,priority:2;check:chk_q_case_party_kind,kind IN ('account','exit')"`
	AccountID      uint64    `gorm:"not null;default:0;uniqueIndex:uidx_q_case_party_ref,priority:3"`
	NodeID         uint64    `gorm:"not null;default:0;uniqueIndex:uidx_q_case_party_ref,priority:4"`
	Epoch          uint64    `gorm:"not null;default:0;uniqueIndex:uidx_q_case_party_ref,priority:5"`
	Role           string    `gorm:"size:16;not null;check:chk_q_case_party_role,role IN ('defendant','co_remanded')"`
	Disposition    string    `gorm:"size:16;not null;check:chk_q_case_party_disposition,disposition IN ('remanded','released','sentenced','withdrawn','dismissed')"`
	UpdatedAt      time.Time `gorm:"not null"`
}

func (qCasePartyModel) TableName() string { return "q_case_party" }

// qDegradeLedgerModel 节点降智台账(G8/B3 决议3:永久保留,
// 流行病学管理数据,不影响调度资格)。节点看总数,历史看 IP 明细。
type qDegradeLedgerModel struct {
	NodeID  uint64    `gorm:"primaryKey"`
	Epoch   uint64    `gorm:"primaryKey;default:0"`
	IP      string    `gorm:"column:ip;size:64;primaryKey;check:chk_q_degrade_ledger_ip,length(ip) <= 64"`
	Count   int64     `gorm:"not null;default:0;check:chk_q_degrade_ledger_count,count >= 0"`
	FirstAt time.Time `gorm:"not null"`
	LastAt  time.Time `gorm:"not null;index:idx_q_degrade_ledger_last"`
}

func (qDegradeLedgerModel) TableName() string { return "q_degrade_ledger" }

// qIPEpochModel 出口 IP 档案(B3):每节点按 epoch 追加行,
// IP 变化即翻篇(I15)——统一 ban 律的执行数据。
type qIPEpochModel struct {
	NodeID uint64 `gorm:"primaryKey"`
	Epoch  uint64 `gorm:"primaryKey;default:0"`
	// CurrentIP 保持聚合展示口径(IPv4 优先);CurrentIPv6 是双族身份的
	// IPv6 侧。升级前旧行 current_ipv6=''(AutoMigrate 加列,默认空串),
	// 首次观测到 IPv6 时按采纳规则补写基线,不翻 epoch。
	CurrentIP   string    `gorm:"column:current_ip;size:64;not null;default:'';check:chk_q_ip_epoch_ip,length(current_ip) <= 64"`
	CurrentIPv6 string    `gorm:"column:current_ipv6;size:64;not null;default:'';check:chk_q_ip_epoch_ipv6,length(current_ipv6) <= 64"`
	FirstSeenAt time.Time `gorm:"not null"`
	ChangedAt   time.Time `gorm:"not null"`
}

func (qIPEpochModel) TableName() string { return "q_ip_epoch" }

// qProbeTaskModel 调查局任务队列(B3):方向/陪审员/被告,状态,结果。
type qProbeTaskModel struct {
	ProjectionVersion  int        `gorm:"not null;default:0;index"`
	ExperimentJSON     string     `gorm:"type:text;not null;default:''"`
	LeaseOwner         string     `gorm:"size:128;not null;default:'';index"`
	LeaseUntil         *time.Time `gorm:"index"`
	AttemptJSON        string     `gorm:"type:text;not null;default:''"`
	ControlAttemptJSON string     `gorm:"type:text;not null;default:''"`
	ID                 uint64     `gorm:"primaryKey;autoIncrement"`
	CaseID             uint64     `gorm:"not null;default:0;index:idx_q_probe_task_case"`
	Direction          string     `gorm:"size:32;not null;check:chk_q_probe_task_direction,direction IN ('account_differential','exit_jury')"`
	DefendantAccountID uint64     `gorm:"not null;default:0"`
	DefendantNodeID    uint64     `gorm:"not null;default:0"`
	DefendantEpoch     uint64     `gorm:"not null;default:0"`
	BaselineNodeID     uint64     `gorm:"not null;default:0"`
	BaselineEpoch      uint64     `gorm:"not null;default:0"`
	JurorAccountID     uint64     `gorm:"not null;default:0"`
	ControlAccountID   uint64     `gorm:"not null;default:0"`
	ControlNodeID      uint64     `gorm:"not null;default:0"`
	ControlEpoch       uint64     `gorm:"not null;default:0"`
	FailureKind        string     `gorm:"size:48;not null;default:''"`
	PathKey            string     `gorm:"size:64;not null;default:''"`
	ControlOutcome     string     `gorm:"size:16;not null;default:''"`
	ControlDetail      string     `gorm:"size:512;not null;default:''"`
	ControlPathKey     string     `gorm:"size:64;not null;default:''"`
	ControlVerified    bool       `gorm:"not null;default:false"`
	State              string     `gorm:"size:16;not null;default:pending;index:idx_q_probe_task_state_updated,priority:1;check:chk_q_probe_task_state,state IN ('pending','running','done','failed','cancelled')"`
	Result             string     `gorm:"size:16;not null;default:'';check:chk_q_probe_task_result,result IN ('','clean','degraded','error')"`
	// VerifiedIPChange is persisted with the result so a later court pass does
	// not have to trust an in-memory executor flag. Unverified differential
	// degradation can never become an account vote after a restart.
	VerifiedIPChange bool      `gorm:"not null;default:false"`
	Detail           string    `gorm:"size:512;not null;default:'';check:chk_q_probe_task_detail,length(detail) <= 512"`
	CreatedAt        time.Time `gorm:"not null"`
	UpdatedAt        time.Time `gorm:"not null;index:idx_q_probe_task_state_updated,priority:2"`
	FinishedAt       *time.Time
}

func (qProbeTaskModel) TableName() string { return "q_probe_task" }

var qualitySchemaModels = append([]any{
	&qAccountStateModel{},
	&qExitStateModel{},
	&qIdentityGroupModel{},
	&qCaseModel{},
	&qCasePartyModel{},
	&qDegradeLedgerModel{},
	&qIPEpochModel{},
	&qNodeEpochModel{},
	&qIncidentClosureModel{},
	&qProbeTaskModel{},
	&qProbeProjectionModel{},
	&qStateRevisionModel{},
	&qCoordinationModel{},
}, append(journal.Models(), evidence.Models()...)...)
