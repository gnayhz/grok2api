package gateway

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func bFormStream() string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("word ", 40) + "\"},\"finish_reason\":null,\"index\":0}]}" + "\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"usage\":{\"completion_tokens\":50,\"completion_tokens_details\":{\"reasoning_tokens\":40}}}" + "\n\n" +
		"data: [DONE]" + "\n\n"
}

func aFormStream() string {
	return "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan\"}}]}" + "\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"good answer\"}}]}" + "\n\n" +
		"data: {\"usage\":{\"completion_tokens\":30,\"completion_tokens_details\":{\"reasoning_tokens\":12}}}" + "\n\n" +
		"data: [DONE]" + "\n\n"
}

type sameAccountFixture struct {
	database    *relational.Database
	accountRepo *relational.AccountRepository
	adapter     *scriptedBuildAdapter
	credentials []accountdomain.Credential
	clientKey   clientkey.Key
}

func newSameAccountFixture(t *testing.T) *sameAccountFixture {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "same-account.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	fixture := &sameAccountFixture{
		database:    database,
		accountRepo: relational.NewAccountRepository(database),
	}
	for index, name := range []string{"same-acct-a", "same-acct-b"} {
		credential, _, createErr := fixture.accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			EncryptedRefreshToken: "refresh-" + name, ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 200 - index, MaxConcurrent: 2,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		fixture.credentials = append(fixture.credentials, credential)
	}
	modelRepo := relational.NewModelRepository(database)
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range fixture.credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	keyRepo := relational.NewClientKeyRepository(database)
	fixture.clientKey, err = keyRepo.Create(ctx, clientkey.Key{
		Name: "same-account-key", Prefix: "sacct", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.adapter = &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	return fixture
}

func (f *sameAccountFixture) scriptAccount(index int, body string) {
	f.adapter.responses[f.credentials[index].ID] = append(f.adapter.responses[f.credentials[index].ID], scriptedBuildResponse{status: http.StatusOK, body: body})
}

// scriptAccountPool 在旋转代理池出口（每请求换新 IP）下投递该响应：同号
// 重试仅在池出口下生效（蓝图 #9），测试用它构造差分重试的前提条件。
func (f *sameAccountFixture) scriptAccountPool(index int, body string) {
	f.adapter.responses[f.credentials[index].ID] = append(f.adapter.responses[f.credentials[index].ID], scriptedBuildResponse{status: http.StatusOK, body: body, poolEgress: true})
}

func (f *sameAccountFixture) service(t *testing.T, cfg QualityRetryRuntime) *Service {
	t.Helper()
	auditRepo := relational.NewAuditRepository(f.database)
	modelRepo := relational.NewModelRepository(f.database)
	responseRepo := relational.NewResponseRepository(f.database)
	registry := provider.NewRegistry(f.adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(f.accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(f.accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 6)
	service.UpdateQualityRetry(cfg)
	return service
}

func (f *sameAccountFixture) request() Input {
	return Input{
		RequestID: "req-same-account", ClientKey: f.clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte("{\"model\":\"grok-4.6\",\"messages\":[{\"role\":\"user\",\"content\":\"answer\"}],\"stream\":true}"),
	}
}

func (f *sameAccountFixture) assertAttempts(t *testing.T, index, want int) {
	t.Helper()
	var got int
	for _, id := range f.adapter.Attempts() {
		if id == f.credentials[index].ID {
			got++
		}
	}
	if got != want {
		t.Fatalf("account %d attempts = %d, want %d", index, got, want)
	}
}

func (f *sameAccountFixture) assertCooldown(t *testing.T, index int, wantCooled bool) {
	t.Helper()
	account, err := f.accountRepo.Get(context.Background(), f.credentials[index].ID)
	if err != nil {
		t.Fatal(err)
	}
	if wantCooled && (account.CooldownUntil == nil || account.LastError != lastErrorMissingThinking) {
		t.Fatalf("account %d should be cooled with missing-thinking", index)
	}
	if !wantCooled && account.CooldownUntil != nil {
		t.Fatalf("account %d should not be cooled, until=%v", index, account.CooldownUntil)
	}
}

func baseSameAccountRuntime() QualityRetryRuntime {
	return QualityRetryRuntime{
		Enabled: true, MaxAttempts: 3, OnExhausted: qualityRetryFailClosed,
		AccountCooldown: time.Hour, SameAccountRetry: true,
	}
}

func TestSameAccountRetryDeliversAndSkipsPenalty(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	fixture.scriptAccountPool(0, bFormStream())
	fixture.scriptAccount(0, aFormStream())
	fixture.scriptAccount(1, aFormStream())

	result, err := fixture.service(t, baseSameAccountRuntime()).CreateChatCompletion(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("same-account retry should deliver, err=%v", err)
	}
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !strings.Contains(string(body), "good answer") || !strings.Contains(string(body), "reasoning_content") {
		t.Fatal("delivered body must be the retried A-form stream")
	}
	if strings.Contains(string(body), "word word word word") {
		t.Fatal("degraded first attempt must not leak")
	}
	fixture.assertCooldown(t, 0, false)
	fixture.assertAttempts(t, 0, 2)
	fixture.assertAttempts(t, 1, 0)
}

func TestSameAccountRetryExhaustedSwitchesAccount(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	fixture.scriptAccountPool(0, bFormStream())
	fixture.scriptAccount(0, bFormStream())
	fixture.scriptAccount(1, aFormStream())

	result, err := fixture.service(t, baseSameAccountRuntime()).CreateChatCompletion(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("switch should deliver, err=%v", err)
	}
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !strings.Contains(string(body), "good answer") {
		t.Fatal("delivered body should come from the second account")
	}
	fixture.assertCooldown(t, 0, true)
	fixture.assertAttempts(t, 0, 2)
	fixture.assertAttempts(t, 1, 1)
}

// TestSameAccountRetryForceDisabledUnderFixedEgress 蓝图 #9：直连/固定
// 出口下同号重试恢复概率≈0（再次进入同一脏 IP），必须强制禁用——扣留
// 直接惩罚换号，不浪费请求预算在同号重试上。
func TestSameAccountRetryForceDisabledUnderFixedEgress(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	// 直连（无池出口记录）：第一次扣留后不得发起同号重试。
	fixture.scriptAccount(0, bFormStream())
	fixture.scriptAccount(1, aFormStream())

	result, err := fixture.service(t, baseSameAccountRuntime()).CreateChatCompletion(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("fixed-egress switch should deliver, err=%v", err)
	}
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !strings.Contains(string(body), "good answer") {
		t.Fatal("delivered body should come from the second account")
	}
	fixture.assertCooldown(t, 0, true)
	fixture.assertAttempts(t, 0, 1)
	fixture.assertAttempts(t, 1, 1)
}

func TestSameAccountRetryDisabledKeepsSwitchBehavior(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	fixture.scriptAccount(0, bFormStream())
	fixture.scriptAccount(1, aFormStream())

	cfg := baseSameAccountRuntime()
	cfg.SameAccountRetry = false
	result, err := fixture.service(t, cfg).CreateChatCompletion(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("deliver expected, err=%v", err)
	}
	_ = result.Body.Close()
	fixture.assertCooldown(t, 0, true)
	fixture.assertAttempts(t, 0, 1)
	fixture.assertAttempts(t, 1, 1)
}

// TestProbeLaneNeverSameAccountRetries closes the deferred audit P0: when a
// probe-lane account (quota recovery due) returns a degraded stream, the
// same-account retry must be skipped — probe-origin accounts exit the retry
// chain straight into penalty/switch. The session-level contract is covered
// by selector_probe_origin_test; this is the request-loop proof (the audit's
// E2E gap: probe conversion flips lease.QuotaProbe, only
// wasQuotaProbeCandidate keeps the exclusion).
func TestProbeLaneNeverSameAccountRetries(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	// credentials[1] enters the quota-probe lane: exhausted recovery with a
	// due probe timestamp (same seeding shape as the selector recovery tests).
	if err := fixture.accountRepo.SaveQuotaRecovery(ctx, accountdomain.QuotaRecovery{
		AccountID: fixture.credentials[1].ID, Kind: accountdomain.QuotaRecoveryKindFree,
		Status:      accountdomain.QuotaRecoveryStatusExhausted,
		ExhaustedAt: &now, NextProbeAt: &due, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Normal account: hard 500 → excluded, loop falls through to the probe lane.
	// Normal account: healthy stream - the switch after the probe withhold
	// must deliver from it, proving the chain went probe-withhold -> switch.
	fixture.scriptAccount(0, aFormStream())
	// Probe account: TWO degraded scripts — if a same-account retry wrongly
	// fires, the second script is consumed and attempts hits 2.
	fixture.scriptAccount(1, bFormStream())
	fixture.scriptAccount(1, bFormStream())

	cfg := baseSameAccountRuntime()
	cfg.MaxAttempts = 6
	result, err := fixture.service(t, cfg).CreateChatCompletion(ctx, fixture.request())
	if err != nil {
		t.Fatalf("switch after probe withhold must deliver from the normal account, err=%v", err)
	}
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !strings.Contains(string(body), "good answer") {
		t.Fatal("delivered body must come from the healthy normal account")
	}
	// Core invariant: the probe-lane account was attempted EXACTLY once -
	// same-account retry must never re-queue it.
	fixture.assertAttempts(t, 1, 1)
	fixture.assertAttempts(t, 0, 1)
	fixture.assertCooldown(t, 1, true)
}

func TestCommitableSameAccountRetryRequiresPool(t *testing.T) {
	t.Parallel()
	cfgOn := QualityRetryRuntime{SameAccountRetry: true}
	cfgOff := QualityRetryRuntime{SameAccountRetry: false}
	session := &selectionSession{}
	cases := []struct {
		name       string
		cfg        QualityRetryRuntime
		used       bool
		quotaProbe bool
		selection  *selectionSession
		poolEgress bool
		want       bool
	}{
		{name: "rotating pool", cfg: cfgOn, selection: session, poolEgress: true, want: true},
		{name: "fixed proxy not pool", cfg: cfgOn, selection: session, poolEgress: false, want: false},
		{name: "flag off even on pool", cfg: cfgOff, selection: session, poolEgress: true, want: false},
		{name: "already used", cfg: cfgOn, used: true, selection: session, poolEgress: true, want: false},
		{name: "quota probe origin", cfg: cfgOn, quotaProbe: true, selection: session, poolEgress: true, want: false},
		{name: "nil selection", cfg: cfgOn, poolEgress: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := commitableSameAccountRetry(tc.cfg, tc.used, tc.quotaProbe, tc.selection, tc.poolEgress)
			if got != tc.want {
				t.Fatalf("commitableSameAccountRetry = %v, want %v", got, tc.want)
			}
		})
	}
}
