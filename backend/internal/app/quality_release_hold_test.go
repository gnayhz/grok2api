package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

// TestCourtReleaseLiftsAccountQualityHold 锚定"无罪即恢复"的整条链:法院放人 →
// 组合根安装的通知(installCourtReleaseNotification) → 账号应用层
// ClearQualityHold → 质量标记与随它而来的瞬态冷却消失。
//
// 反向契约同样锚定:只要还有另一个案件羁押该账号,释放一个案件不得提前解除
// 它的质量标记——否则一个案件就能替另一个案件解禁。
func TestCourtReleaseLiftsAccountQualityHold(t *testing.T) {
	ctx := context.Background()
	a := newLifecycleApplication(t)
	if err := a.qualityCourt.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// 记录派发而不真正执行探针:本用例只关心释放通知,取证路径不得触碰网络。
	dispatch := &captureCandidatePlan{}
	cfg := court.DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	a.qualityCourt = court.New(cfg, a.quality, qualityEvidenceSource{store: a.qualityEvidence}, dispatch, registry.NewProbeTaskStore(a.quality))
	a.qualityCourt.SetNodes(baseNodeSource{egress: a.egressOps})
	a.qualityCourt.SetProbeAccounts(a.gateway)
	installCourtReleaseNotification(a.qualityCourt, a.accounts)

	cooldownUntil := time.Now().UTC().Add(time.Hour)
	marked, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "exonerated", SourceKey: "exonerated",
		Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "unused",
		FailureCount: 3, CooldownUntil: &cooldownUntil, CooldownMarkedAt: &cooldownUntil,
		LastError: account.LastErrorQualityIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, marked.ID, []string{"grok-4.6"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}

	// Two independent incidents on two exits: the account is detained by both.
	for _, nodeID := range []uint64{201, 202} {
		eventID := fmt.Sprintf("release-hold-%d", nodeID)
		obs := model.Observation{
			At: time.Now(), AccountID: marked.ID, Exit: model.EpochKey{NodeID: nodeID},
			Source: model.SourceTraffic, Outcome: model.OutcomeDegraded, EventID: eventID,
			Attempt: attemptmeta.Identity{ID: eventID, Provider: "grok_build", Model: "grok-4.6", RuleVersion: "reasoning-v1", Profile: attemptmeta.Profile{Known: true, Protocol: "responses"}},
		}
		if err := a.qualityCourt.ReportDegradedObservation(ctx, obs); err != nil {
			t.Fatal(err)
		}
	}

	detained, err := a.accountRepo.Get(ctx, marked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detained.LastError != account.LastErrorQualityIdle || detained.CooldownUntil == nil {
		t.Fatalf("opening cases must leave the quality hold in place: lastError=%q cooldown=%v", detained.LastError, detained.CooldownUntil)
	}

	cases, err := a.quality.ListOpenCases(ctx)
	if err != nil || len(cases) != 2 {
		t.Fatalf("expected two open cases, cases=%d err=%v", len(cases), err)
	}
	caseByExit := make(map[uint64]uint64, len(cases))
	for _, record := range cases {
		parties, err := a.quality.ListParties(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, party := range parties {
			if party.Kind == model.PartyExit {
				caseByExit[party.NodeID] = record.ID
			}
		}
	}
	if caseByExit[201] == 0 || caseByExit[202] == 0 {
		t.Fatalf("cases not mapped to their exits: %v", caseByExit)
	}

	// Releasing one case leaves the second case authoritative.
	if err := a.qualityCourt.ReleaseAfterReview(ctx, caseByExit[201], "operator review"); err != nil {
		t.Fatal(err)
	}
	stillHeld, err := a.accountRepo.Get(ctx, marked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillHeld.LastError != account.LastErrorQualityIdle || stillHeld.CooldownUntil == nil {
		t.Fatalf("a second open case still detains the account: lastError=%q cooldown=%v", stillHeld.LastError, stillHeld.CooldownUntil)
	}

	// The last holder releases it: the quality hold must go immediately.
	if err := a.qualityCourt.ReleaseAfterReview(ctx, caseByExit[202], "operator review"); err != nil {
		t.Fatal(err)
	}
	released, err := a.accountRepo.Get(ctx, marked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.LastError != "" || released.CooldownUntil != nil {
		t.Fatalf("exonerated account kept its quality hold: lastError=%q cooldown=%v", released.LastError, released.CooldownUntil)
	}
}
