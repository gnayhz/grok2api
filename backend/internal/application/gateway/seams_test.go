package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// stubAccountEligibility 可编程账号资格谓词。
type stubAccountEligibility struct {
	ineligible map[uint64]bool
}

func (s stubAccountEligibility) AccountSchedulable(accountID uint64) bool {
	return !s.ineligible[accountID]
}

func newSeamSelectorDB(t *testing.T, names ...string) (*relational.Database, []account.Credential) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-seam.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credentials := make([]account.Credential, 0, len(names))
	for index, name := range names {
		credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100 - index, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		credentials = append(credentials, credential)
	}
	return database, credentials
}

// TestSelectorEligibilitySeamSkipsIneligible 锚定 D3-1:资格谓词缝隙
// 把质量层判定的不可调度账号移出生产调度;缝隙未注入时行为不变
// (既有全套 selector 测试即零变化证明)。
func TestSelectorEligibilitySeamSkipsIneligible(t *testing.T) {
	ctx := context.Background()
	database, credentials := newSeamSelectorDB(t, "seam-a", "seam-b")
	accounts := relational.NewAccountRepository(database)
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	selector.SetQualityEligibility(stubAccountEligibility{ineligible: map[uint64]bool{credentials[0].ID: true}})
	lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != credentials[1].ID {
		t.Fatalf("缝隙必须移出不可调度账号: lease=%d, want %d", lease.Credential.ID, credentials[1].ID)
	}
	lease.Release()
	// 全部不可调度 → 无账号可选。
	selector.SetQualityEligibility(stubAccountEligibility{ineligible: map[uint64]bool{credentials[0].ID: true, credentials[1].ID: true}})
	if _, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true); err == nil {
		t.Fatal("全部不可调度时必须无账号可选")
	}
}

// TestSelectorEligibilityPinnedAndProbeBypass 锚定 B2 生产/探孔双通道:
// 生产钉住尊重质量资格;取证探测(AcquirePinnedForQualityProbe)放行。
func TestSelectorEligibilityPinnedAndProbeBypass(t *testing.T) {
	ctx := context.Background()
	database, credentials := newSeamSelectorDB(t, "seam-pinned")
	accounts := relational.NewAccountRepository(database)
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	selector.SetQualityEligibility(stubAccountEligibility{ineligible: map[uint64]bool{credentials[0].ID: true}})
	if _, err := selector.AcquirePinned(ctx, account.ProviderBuild, credentials[0].ID, 0, "grok-test", "", true); err == nil {
		t.Fatal("生产钉住必须尊重质量资格")
	}
	scope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeAll}
	lease, err := selector.AcquirePinnedForQualityProbe(ctx, account.ProviderBuild, credentials[0].ID, 0, "grok-test", "", scope)
	if err != nil {
		t.Fatalf("取证探测不受资格限制(B2 双通道): %v", err)
	}
	lease.Release()
}

// recordingQualityObserver 捕获缝隙观测用于断言。
type recordingQualityObserver struct {
	events []QualityObservation
}

func (r *recordingQualityObserver) RecordQualityObservation(obs QualityObservation) {
	r.events = append(r.events, obs)
}

// stubRetryPolicy 可编程重试策略缝隙。
type stubRetryPolicy struct{ action QualityRetryAction }

func (s stubRetryPolicy) DecideQualityRetry(verdict QualityVerdict, attemptIndex, maxAttempts int, onExhausted string) QualityRetryAction {
	return s.action
}

// TestRetryPolicySeamRoutesDecision 锚定 D3-3b:策略缝隙注入后,
// "再试一次"决策由质量层给出,底座仍做路由边界兜底。
func TestRetryPolicySeamRoutesDecision(t *testing.T) {
	service := &Service{}
	// 未注入:内建策略(现行行为)。
	commit := service.decideQualityCommit(QualityWithhold, 0, 2, true, qualityRetryFailClosed)
	if commit.Action != QualityActionRetry {
		t.Fatalf("内建策略首扣留应重试: %+v", commit)
	}
	// 注入:策略决定。
	service.SetQualityRetryPolicy(stubRetryPolicy{action: QualityActionReject})
	commit = service.decideQualityCommit(QualityWithhold, 0, 2, true, qualityRetryFailClosed)
	if commit.Action != QualityActionReject || !commit.Audit {
		t.Fatalf("策略缝隙必须接管决策: %+v", commit)
	}
	// 路由边界兜底:策略说 retry 但无下一跳时收敛为 reject(fail-closed)。
	service.SetQualityRetryPolicy(stubRetryPolicy{action: QualityActionRetry})
	commit = service.decideQualityCommit(QualityWithhold, 1, 2, false, qualityRetryFailClosed)
	if commit.Action != QualityActionReject {
		t.Fatalf("无下一跳时不得空转重试: %+v", commit)
	}
}

// TestQualityObserverSeamNonBlocking 锚定 I19(缝隙侧):观察点回调
// 必须立即返回；这是可选遥测端口，生产必要回执另走持久事件接口。
func TestQualityObserverSeamNonBlocking(t *testing.T) {
	observer := &recordingQualityObserver{}
	service := &Service{}
	service.SetQualityObserver(observer)
	done := make(chan struct{})
	go func() {
		defer close(done)
		loaded := service.qualityObservationObserver()
		if loaded != nil {
			loaded.RecordQualityObservation(QualityObservation{AccountID: 1, NodeID: 2, Outcome: QualityObservedDegraded, Rule: "seam"})
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("观察点回调不得阻塞")
	}
	if len(observer.events) != 1 || observer.events[0].Outcome != QualityObservedDegraded {
		t.Fatalf("观测 = %+v", observer.events)
	}
}
