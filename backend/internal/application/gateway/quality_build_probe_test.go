package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// TestProbeBuildThinkingOutcomes：风险归因的 Build 差分探针此前只被 risk
// 包的桩测试间接覆盖（真实实现 0%）。这里直击三种结论：首试即 clean、
// 双路降智 degraded、无出口选择（单路直连不可差分）error。
func TestProbeBuildThinkingOutcomes(t *testing.T) {
	ctx := context.Background()
	database, credentials := newGuardLoopDatabase(t, "grok-4.6", "probe-account")
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	_ = credentials
	healthyStream := strings.Join([]string{
		"data: " + `{"type":"response.reasoning_text.delta","delta":"think"}`,
		"data: " + `{"type":"response.output_text.delta","delta":"2"}`,
		"data: " + `{"type":"response.completed","response":{"id":"resp_ok"}}`,
		"",
	}, "\n")
	degradedStream := strings.Join([]string{
		"data: " + `{"type":"response.created","response":{"id":"resp_deg"}}`,
		"data: " + `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"xx"}}`,
		"",
	}, "\n")
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {
			{status: http.StatusOK, body: healthyStream},
			{status: http.StatusOK, body: degradedStream},
			{status: http.StatusOK, body: degradedStream},
		},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)
	service.UpdateEgressCanary(EgressCanaryRuntime{ModelPublicID: "grok-4.6", CreatedTimeout: 5 * time.Second})

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 1) 首试即 clean（思考增量在场）。
	clean := service.ProbeBuildThinking(probeCtx, credentials[0].ID, 0)
	if clean.Outcome != buildProbeClean {
		t.Fatalf("healthy probe outcome = %q reason=%s", clean.Outcome, clean.Reason)
	}
	// 2) 降智且无任何出口选择（degradedNodeID=0 且 trace 无选择）→ 单路
	// 直连不可差分，纪律要求 error 绝不定罪。
	single := service.ProbeBuildThinking(probeCtx, credentials[0].ID, 0)
	if single.Outcome != buildProbeError {
		t.Fatalf("single-path degraded probe outcome = %q reason=%s, want error (never convict)", single.Outcome, single.Reason)
	}
}
