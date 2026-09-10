package model

// CaseStatus 是案件生命周期状态。每个案件对应一个具体的降智事件;
// 被告是账号,共同当事方是降智发生地的出口。
type CaseStatus string

const (
	// CaseInvestigating 调查中:立案后等待本轮有限探针结束。
	CaseInvestigating CaseStatus = "investigating"
	// CaseAccountGuilty 账号有罪:被告账号(身份组连坐)定罪服刑。
	CaseAccountGuilty CaseStatus = "account_guilty"
	// CaseExitGuilty IP 有罪:被告出口按节点型 ban,账号全抹释放。
	CaseExitGuilty CaseStatus = "exit_guilty"
	// CaseDismissed 证据不足:双方释放并关闭案件。
	CaseDismissed CaseStatus = "dismissed"
)

// Closed 报告案件是否已终结。
func (s CaseStatus) Closed() bool {
	return s != CaseInvestigating
}

// Verdict 是裁决词汇(B1.3 裁决分支)。
type Verdict string

const (
	// VerdictNone 未裁决。
	VerdictNone Verdict = ""
	// VerdictAccountGuilty 账号有罪。
	VerdictAccountGuilty Verdict = "account_guilty"
	// VerdictExitGuilty 出口(IP)有罪。
	VerdictExitGuilty Verdict = "exit_guilty"
	// VerdictInsufficient 证据不足:双方释放,案件关闭。
	VerdictInsufficient Verdict = "insufficient"
	// VerdictDismissed 兼容历史词汇;当前有限闭环与 insufficient 一起关闭案件。
	VerdictDismissed Verdict = "dismissed"
)

// PartyKind 案件当事方类别。
type PartyKind string

const (
	// PartyAccount 账号方(被告)。
	PartyAccount PartyKind = "account"
	// PartyExit 出口方(共同被押的降智发生地)。
	PartyExit PartyKind = "exit"
)

// PartyRole 当事方在案件中的角色。
type PartyRole string

const (
	// RoleDefendant 被告:降智事件直接关联方。
	RoleDefendant PartyRole = "defendant"
	// RoleCoRemanded 共同被押:被告账号降智时所在的出口。
	RoleCoRemanded PartyRole = "co_remanded"
)

// PartyDisposition 当事方的程序处置状态(区别于调度状态:
// 取保/释放是程序语义,调度资格由 AccountState/ExitState 决定)。
type PartyDisposition string

const (
	// DispositionRemanded 羁押中。
	DispositionRemanded PartyDisposition = "remanded"
	// DispositionReleased 无罪释放(全抹除)。
	DispositionReleased PartyDisposition = "released"
	// DispositionSentenced 定罪服刑(账号方)。
	DispositionSentenced PartyDisposition = "sentenced"
	// DispositionWithdrawn 出口方因 epoch 翻篇自动退出案件
	// (统一 ban 律:新 IP 不继承旧嫌疑;账号方继续审理)。
	DispositionWithdrawn PartyDisposition = "withdrawn"
	// DispositionDismissed 恢复销案(无痕)。
	DispositionDismissed PartyDisposition = "dismissed"
)
