package model

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// Records are quality facts exchanged by use cases and persistence ports.
// Conditional writes, leases and SQL schemas remain in registry.
// IncidentKey names one frozen account/exit baseline, including direct traffic.
type IncidentKey struct {
	AccountID uint64
	Exit      EpochKey
}

// ExitTransitionRequest 描述一次出口状态转移(必须指向当前 epoch)。
type ExitTransitionRequest struct {
	NodeID uint64
	// Epoch 必须等于节点当前 epoch，旧裁决不得施加到新 IP。
	Epoch uint64
	To    ExitState
	// CaseID 案件号:REMANDED/BANNED 目标必须非零。
	CaseID uint64
}

// ExitIPRecord 是 IP 档案的一行。IP 保持聚合展示口径(IPv4 优先),
// IPv6 是双族身份的 IPv6 侧(旧档案可能为空)。
type ExitIPRecord struct {
	Epoch       uint64
	IP          string
	IPv6        string
	FirstSeenAt time.Time
	ChangedAt   time.Time
}

// NodeQualityView 是节点质量面的面板投影(IP 轮换入口):
// 当前 epoch/IP、台账总数与各 IP 明细。
type NodeQualityView struct {
	NodeID        uint64               `json:"node_id"`
	CurrentEpoch  uint64               `json:"current_epoch"`
	CurrentIP     string               `json:"current_ip"`
	State         ExitState            `json:"state"`
	DegradeTotal  int64                `json:"degrade_total"`
	DegradeDetail []DegradeLedgerEntry `json:"degrade_detail"`
}

// AccountEntry 是热缓存中的账号质量状态条目。
type AccountEntry struct {
	State         AccountState
	StateSince    time.Time
	CurrentCaseID uint64
}

// ExitEntry 是热缓存中的出口质量状态条目(按 EpochKey 键控)。
type ExitEntry struct {
	State         ExitState
	StateSince    time.Time
	CurrentCaseID uint64
}

// AccountTransitionRequest describes one direct-loop state transition.
type AccountTransitionRequest struct {
	AccountID uint64
	To        AccountState
	CaseID    uint64
}

// ProbeTaskView 是探针任务的面板投影。
type ProbeTaskView struct {
	ResourceCheck    *ResourceCheckReport
	Experiment       ProbeExperiment
	Attempt          attemptmeta.Identity
	ControlAttempt   attemptmeta.Identity
	ControlAccountID uint64
	ControlNodeID    uint64
	ControlEpoch     uint64
	FailureKind      string
	PathKey          string
	ControlOutcome   ProbeResult
	ControlDetail    string
	ControlPathKey   string
	ControlVerified  bool
	ID               uint64
	CaseID           uint64
	Direction        ProbeDirection
	Defendant        uint64
	NodeID           uint64
	Epoch            uint64
	BaselineNodeID   uint64
	BaselineEpoch    uint64
	Juror            uint64
	State            ProbeTaskState
	Result           ProbeResult
	VerifiedIPChange bool
	Detail           string
	CreatedAt        time.Time
	FinishedAt       *time.Time
}

// DegradeLedgerEntry 是台账的一行:(节点,epoch,IP) 聚合。
// JSON 字段名是前端 degrade_detail 的解码契约。
type DegradeLedgerEntry struct {
	NodeID  uint64    `json:"node_id"`
	Epoch   uint64    `json:"epoch"`
	IP      string    `json:"ip"`
	Count   int64     `json:"count"`
	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
}

// CaseRecord 是案件的一行。
type CaseRecord struct {
	ID           uint64
	Status       CaseStatus
	Verdict      Verdict
	EvidenceJSON string
	OpenedAt     time.Time
	ClosedAt     *time.Time
	UpdatedAt    time.Time
}

// PartyRecord 是案件当事方的一行。
type PartyRecord struct {
	CaseID      uint64
	Kind        PartyKind
	AccountID   uint64
	NodeID      uint64
	Epoch       uint64
	Role        PartyRole
	Disposition PartyDisposition
	UpdatedAt   time.Time
}

// ManagementState is one immutable quality-state revision for a management
// projection. Exit states retain their epoch key; a current-node projection
// cannot describe the historical columns of an evidence matrix.
type ManagementState struct {
	Accounts map[uint64]AccountEntry
	Exits    map[EpochKey]ExitEntry
}
