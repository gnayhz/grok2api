package risk

import (
	"context"
	"testing"
)

// 不变量:每条落库的 verdict 都必须携带触发源——含 decrypt 失败的
// error verdict(它不携带后果,但保持字段完整,避免读者猜测 0 的含义)。
func TestDecryptFailureVerdictStampsOrigin(t *testing.T) {
	accounts := newFakeAccounts()
	// 不放 token:DecryptedAccessToken 返回错误。
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	service := New(baseTestConfig(), accounts, store, &fakeChecker{}, nil)

	verdict := service.checkNow(context.Background(), 90, 91)

	if verdict.Verdict != VerdictError {
		t.Fatalf("verdict = %s, want error", verdict.Verdict)
	}
	if verdict.OriginAccountID != 91 {
		t.Fatalf("OriginAccountID = %d, want 91", verdict.OriginAccountID)
	}
	stored, err := store.GetRiskVerdict(context.Background(), 90)
	if err != nil {
		t.Fatal("error verdict must persist for the retry window")
	}
	if stored.OriginAccountID != 91 {
		t.Fatalf("stored OriginAccountID = %d, want 91", stored.OriginAccountID)
	}
}
