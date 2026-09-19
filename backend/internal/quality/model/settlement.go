package model

import "fmt"

// CaseStatusForVerdict 是判决到案件终态的确定性映射（处置纯规则）。SQL
// 事务在锁内调用本函数决定状态字段；SQL 不再各自维护一份映射。
func CaseStatusForVerdict(verdict Verdict) (CaseStatus, error) {
	switch verdict {
	case VerdictAccountGuilty:
		return CaseAccountGuilty, nil
	case VerdictExitGuilty:
		return CaseExitGuilty, nil
	case VerdictBothGuilty:
		return CaseBothGuilty, nil
	case VerdictInsufficient:
		return CaseDismissed, nil
	default:
		return "", fmt.Errorf("invalid experiment verdict %q", verdict)
	}
}

func (v Verdict) RestrictsAccount() bool { return v == VerdictAccountGuilty || v == VerdictBothGuilty }
func (v Verdict) RestrictsExit() bool    { return v == VerdictExitGuilty || v == VerdictBothGuilty }

// ReviewRelease 计算人工复核释放后的下一状态。otherHolderCaseID 非零
// 表示同方存在其他已定罪案件（事实由调用方在当前事务内读取）：
// 保持服刑并指向该持有方；否则账户回候审、出口回羁押。
func ReviewReleaseAccount(otherHolderCaseID, fallbackCaseID uint64) (AccountState, uint64) {
	if otherHolderCaseID != 0 {
		return AccountSentenced, otherHolderCaseID
	}
	return AccountRemanded, fallbackCaseID
}

func ReviewReleaseExit(otherHolderCaseID, fallbackCaseID uint64) (ExitState, uint64) {
	if otherHolderCaseID != 0 {
		return ExitBanned, otherHolderCaseID
	}
	return ExitRemanded, fallbackCaseID
}
