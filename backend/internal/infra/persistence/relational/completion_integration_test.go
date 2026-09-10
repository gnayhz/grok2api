package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCompletionOwnershipIdentityIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			var candidates []inferencedomain.ResponseOwnership
			now := time.Now().UTC()
			for i := 0; i < 2; i++ {
				name := fmt.Sprintf("owner-%d", i)
				credential, _, err := NewAccountRepository(a).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				key, err := NewClientKeyRepository(a).Create(ctx, clientkey.Key{Name: name, Prefix: name, SecretHash: strings.Repeat(fmt.Sprint(i+1), 64), EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
				if err != nil {
					t.Fatal(err)
				}
				candidates = append(candidates, inferencedomain.ResponseOwnership{ResponseID: "colliding-upstream-id", AccountID: credential.ID, ClientKeyID: key.ID, ModelRouteID: uint64(i + 1), Provider: account.ProviderBuild, PromptCacheKey: name, ReasoningReplayKey: name, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now})
			}
			stores := []*ResponseRepository{NewResponseRepository(a), NewResponseRepository(b)}
			start := make(chan struct{})
			results := make([]error, 2)
			var work sync.WaitGroup
			for i := range stores {
				work.Add(1)
				go func(i int) { defer work.Done(); <-start; results[i] = stores[i].Save(ctx, candidates[i]) }(i)
			}
			close(start)
			work.Wait()
			winner := 0
			if results[0] != nil {
				winner = 1
			}
			if results[winner] != nil || !errors.Is(results[1-winner], repository.ErrConflict) {
				t.Fatalf("two writers rebound one identity: %v", results)
			}
			value := candidates[winner]
			if err := stores[1-winner].Save(ctx, value); err != nil {
				t.Fatalf("idempotent acknowledgement: %v", err)
			}
			for _, change := range []func(*inferencedomain.ResponseOwnership){
				func(v *inferencedomain.ResponseOwnership) { v.AccountID = candidates[1-winner].AccountID },
				func(v *inferencedomain.ResponseOwnership) { v.ClientKeyID = candidates[1-winner].ClientKeyID },
				func(v *inferencedomain.ResponseOwnership) { v.ModelRouteID++ },
				func(v *inferencedomain.ResponseOwnership) { v.Provider = account.ProviderConsole },
				func(v *inferencedomain.ResponseOwnership) { v.PromptCacheKey = "different" },
				func(v *inferencedomain.ResponseOwnership) { v.ReasoningReplayKey = "different" },
			} {
				rebound := value
				change(&rebound)
				if err := stores[1-winner].Save(ctx, rebound); !errors.Is(err, repository.ErrConflict) {
					t.Fatalf("identity change allowed: %+v error=%v", rebound, err)
				}
			}
			got, err := stores[1-winner].Get(ctx, value.ResponseID, value.ClientKeyID, now)
			if err != nil || got.AccountID != value.AccountID || got.ModelRouteID != value.ModelRouteID || got.PromptCacheKey != value.PromptCacheKey || got.ReasoningReplayKey != value.ReasoningReplayKey {
				t.Fatalf("original owner was corrupted: %+v %v", got, err)
			}
			if _, err := stores[0].Get(ctx, value.ResponseID, candidates[1-winner].ClientKeyID, now); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("foreign client can retrieve collision: %v", err)
			}
		})
	}
}

func TestCompletionAuditMigrationIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			old := audit.Record{RequestID: "legacy", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC()}
			if err := NewAuditRepository(a).Create(ctx, old); err != nil {
				t.Fatal(err)
			}
			// Reconstruct the pre-D01 columns, then run the real startup migration.
			for _, column := range []string{"response_id", "upstream_status_code", "admission_outcome", "generation_outcome", "provider_state_commit", "ownership_commit", "delivery_outcome", "physical_receipt", "quality_receipt", "ledger_outcome"} {
				if err := a.db.Exec("ALTER TABLE request_audits DROP COLUMN " + column).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := a.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			values, _, err := NewAuditRepository(b).List(ctx, 0, 1)
			if err != nil || len(values) != 1 || values[0].GenerationOutcome != "" || values[0].LedgerOutcome != "" {
				t.Fatalf("migration fabricated historical receipts: %+v %v", values, err)
			}
			value := audit.Record{RequestID: "completion", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 499, CreatedAt: time.Now().UTC(), ResponseID: "resp-persisted",
				AdmissionOutcome: "admitted", GenerationOutcome: "completed", HistoryCommit: "committed", ProviderStateCommit: "failed", OwnershipCommit: "not_committed", DeliveryOutcome: "canceled", PhysicalReceipt: "committed", QualityReceipt: "failed", LedgerOutcome: "failed",
				InputTokens: 20, OutputTokens: 5, TotalTokens: 25, DeliveredBytes: 91, DeliveredEvents: 2, ErrorCode: "client_disconnected"}
			if err := NewAuditRepository(a).Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			// An independent repository/connection reads durable values after the
			// audit/billing transaction; the writer owns the ledger acknowledgement.
			values, _, err = NewAuditRepository(b).List(ctx, 0, 1)
			if err != nil || len(values) != 1 {
				t.Fatalf("receipt read=%+v %v", values, err)
			}
			got := values[0]
			if got.ResponseID != value.ResponseID || got.AdmissionOutcome != value.AdmissionOutcome || got.GenerationOutcome != value.GenerationOutcome || got.HistoryCommit != value.HistoryCommit || got.ProviderStateCommit != value.ProviderStateCommit || got.OwnershipCommit != value.OwnershipCommit || got.DeliveryOutcome != value.DeliveryOutcome || got.PhysicalReceipt != value.PhysicalReceipt || got.QualityReceipt != value.QualityReceipt || got.LedgerOutcome != "committed" || got.DeliveredBytes != 91 || got.InputTokens != 20 {
				t.Fatalf("independent completion facts were lost: %+v", got)
			}
		})
	}
}

func TestCompletionNativeStateIdentityIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			now := time.Now().UTC()
			stores := []*ResponseRepository{NewResponseRepository(a), NewResponseRepository(b)}
			candidates := []inferencedomain.WebResponseState{
				{ResponseID: "colliding-native-id", AccountID: 1, ConversationID: "one", UpstreamParentResponseID: "parent-one", ResponseJSON: `{"id":"colliding-native-id","output":[]}`, Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now},
				{ResponseID: "colliding-native-id", AccountID: 2, ConversationID: "two", UpstreamParentResponseID: "parent-two", ResponseJSON: `{"id":"colliding-native-id","output":["two"]}`, Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now},
			}
			for i := range candidates {
				name := fmt.Sprintf("native-owner-%d", i)
				credential, _, err := NewAccountRepository(a).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: name, SourceKey: name, EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				candidates[i].AccountID = credential.ID
			}
			start := make(chan struct{})
			var work sync.WaitGroup
			results := make([]error, len(stores))
			for i := range stores {
				work.Add(1)
				go func(i int) { defer work.Done(); <-start; results[i] = stores[i].SaveWebState(ctx, candidates[i]) }(i)
			}
			close(start)
			work.Wait()
			winner := 0
			if results[0] != nil {
				winner = 1
			}
			if results[winner] != nil || !errors.Is(results[1-winner], repository.ErrConflict) {
				t.Fatalf("two native writers rebound one identity: %v", results)
			}
			value := candidates[winner]
			for _, change := range []func(*inferencedomain.WebResponseState){
				func(v *inferencedomain.WebResponseState) { v.AccountID = candidates[1-winner].AccountID },
				func(v *inferencedomain.WebResponseState) { v.ConversationID += "changed" },
				func(v *inferencedomain.WebResponseState) { v.UpstreamParentResponseID += "changed" },
				func(v *inferencedomain.WebResponseState) { v.ResponseJSON = "{}" },
				func(v *inferencedomain.WebResponseState) { v.Status = "failed" },
			} {
				rebound := value
				change(&rebound)
				if err := stores[1-winner].SaveWebState(ctx, rebound); !errors.Is(err, repository.ErrConflict) {
					t.Fatalf("native identity change allowed: %+v %v", rebound, err)
				}
			}
			value.ExpiresAt = now.Add(2 * time.Hour)
			value.UpdatedAt = now.Add(time.Minute)
			if err := stores[1-winner].SaveWebState(ctx, value); err != nil {
				t.Fatalf("idempotent acknowledgement: %v", err)
			}
			if err := stores[winner].SaveWebState(ctx, candidates[winner]); err != nil {
				t.Fatal(err)
			}
			got, err := stores[1-winner].GetWebState(ctx, value.ResponseID, now.Add(90*time.Minute))
			if err != nil || got.AccountID != value.AccountID || got.ConversationID != value.ConversationID || got.UpstreamParentResponseID != value.UpstreamParentResponseID || got.ResponseJSON != value.ResponseJSON || got.UpdatedAt.Before(now.Add(59*time.Second)) {
				t.Fatalf("native identity or monotonic expiry lost: %+v %v", got, err)
			}
		})
	}
}

func TestVoiceAuditDurationMigrationIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			legacy := audit.Record{RequestID: "legacy-voice", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, Operation: audit.OperationSTT, UsageSource: audit.UsageSourceNone, CreatedAt: time.Now().UTC()}
			if err := NewAuditRepository(a).Create(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Exec("ALTER TABLE request_audits DROP COLUMN audio_duration_ms").Error; err != nil {
				t.Fatal(err)
			}
			if err := a.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			values, _, err := NewAuditRepository(b).List(ctx, 0, 1)
			if err != nil || len(values) != 1 || values[0].AudioDurationMS != 0 || values[0].UsageSource != audit.UsageSourceNone {
				t.Fatalf("legacy unknown usage: %+v %v", values, err)
			}
			for _, duration := range []int64{0, 1, 3450, 3600000} {
				record := audit.Record{EventID: fmt.Sprintf("voice-duration-%d", duration), RequestID: fmt.Sprintf("voice-duration-%d", duration), ClientKeyID: 1, ModelRouteID: 1, Operation: audit.OperationSTT, UsageSource: audit.UsageSourceUpstream, AudioDurationMS: duration, Streaming: true, GenerationOutcome: "completed", DeliveryOutcome: "canceled", PhysicalReceipt: "committed", StatusCode: 502, CreatedAt: time.Now().UTC()}
				if err := NewAuditRepository(a).Create(ctx, record); err != nil {
					t.Fatal(err)
				}
				values, _, err := NewAuditRepository(b).List(ctx, 0, 1)
				if err != nil || len(values) != 1 {
					t.Fatalf("read duration: %+v %v", values, err)
				}
				detail, err := NewAuditRepository(b).Get(ctx, values[0].ID)
				if err != nil || detail.AudioDurationMS != duration || detail.UsageSource != audit.UsageSourceUpstream || detail.GenerationOutcome != "completed" || detail.DeliveryOutcome != "canceled" || detail.LedgerOutcome != "committed" {
					t.Fatalf("durable voice facts: %+v %v", detail, err)
				}
			}
		})
	}
}
