package management

import "testing"

// TestTunablesRejectImpossibleThresholds 锚定批10 可行性约束:
// 降票数>见证人数或跨节点数>降智出口数
// 都会静默关闭对应定罪通道——必须拒绝保存。
func TestTunablesRejectImpossibleThresholds(t *testing.T) {
	base := DefaultConfig()
	// 基线合法。
	if _, _, err := normalize(base); err != nil {
		t.Fatalf("默认参数应合法: %v", err)
	}
	// k > n:出口定罪不可达。
	badK := base
	badK.ExitNeedK = base.ExitNeedN + 1
	if _, _, err := normalize(badK); err == nil {
		t.Fatal("exit_need_k > exit_need_n 必须拒绝")
	}
	// span > exits:账号定罪不可达。
	badSpan := base
	badSpan.AccountSpanNodes = base.AccountNeedExits + 1
	if _, _, err := normalize(badSpan); err == nil {
		t.Fatal("account_span_nodes > account_need_exits 必须拒绝")
	}
	// 边界合法值不误伤。
	edge := base
	edge.ExitNeedK = base.ExitNeedN
	edge.AccountSpanNodes = base.AccountNeedExits
	if _, _, err := normalize(edge); err != nil {
		t.Fatalf("边界等值应合法: %v", err)
	}
	// 陪审员数量不足以填满出口见证门槛时,出口定罪同样不可达。
	badJurors := base
	badJurors.JurorsPerExit = base.ExitNeedN - 1
	if _, _, err := normalize(badJurors); err == nil {
		t.Fatal("jurors_per_exit < exit_need_n 必须拒绝")
	}
	// 差分与陪审核心任务必须共存于同一轮预算内。
	badBudget := base
	badBudget.ProbeBudget = base.DifferentialExits + base.JurorsPerExit - 1
	if _, _, err := normalize(badBudget); err == nil {
		t.Fatal("probe_budget 不足以覆盖核心任务时必须拒绝")
	}
}

func TestPersistedLegacyDefaultsAndInvalidRows(t *testing.T) {
	value, err := decodePersisted([]byte(`{"account_need_exits":0,"retention":"","probe_budget":14}`))
	if err != nil || value.AccountNeedExits != DefaultConfig().AccountNeedExits || value.ProbeBudget != 14 {
		t.Fatalf("legacy defaults=%+v %v", value, err)
	}
	for _, payload := range []string{
		`{"_schema_version":2,"account_need_exits":0}`,
		`{"_schema_version":99}`, `{"probe_budget":-1}`, `{"evidence_window":"invalid"}`,
	} {
		if _, err := decodePersisted([]byte(payload)); err == nil {
			t.Fatalf("invalid document accepted: %s", payload)
		}
	}
}
