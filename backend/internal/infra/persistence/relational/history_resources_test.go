package relational

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestHistoryResourcesTwoInstancesRetentionAndConcurrentDelete(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			a, b := settingsDatabasePair(t, dialect)
			repoA, repoB := NewResponseRepository(a), NewResponseRepository(b)
			first, second := historyapp.NewResponseResources(repoA), historyapp.NewResponseResources(repoB)
			owner, _, err := NewAccountRepository(a).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "resource", SourceKey: "resource", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			key, err := NewClientKeyRepository(a).Create(ctx, clientkey.Key{Name: "resource", Prefix: "resource", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().In(time.FixedZone("positive", 8*3600)).Truncate(time.Millisecond)
			value := inferencedomain.ResponseOwnership{ResponseID: "resource", AccountID: owner.ID, ClientKeyID: key.ID, ModelRouteID: 42, Provider: owner.Provider, PromptCacheKey: "hint", ReasoningReplayKey: "replay"}
			if err = first.Record(ctx, value, now); err != nil {
				t.Fatal(err)
			}
			saved, err := second.Lookup(ctx, value.ResponseID, key.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if !saved.ExpiresAt.Equal(now.Add(30*24*time.Hour)) || !saved.CreatedAt.Equal(now) || saved.PromptCacheKey != value.PromptCacheKey || saved.ReasoningReplayKey != value.ReasoningReplayKey {
				t.Fatalf("record policy changed: %+v", saved)
			}
			if _, err = second.Lookup(ctx, value.ResponseID, key.ID, now.Add(30*24*time.Hour)); !errors.Is(err, historydomain.ErrResponseNotFound) {
				t.Fatal(err)
			}
			if _, err = second.Lookup(ctx, value.ResponseID, key.ID+1, now); !errors.Is(err, historydomain.ErrResponseNotFound) {
				t.Fatal(err)
			}
			if err = first.Forget(ctx, value.ResponseID, key.ID+1); err != nil {
				t.Fatal(err)
			}
			if _, err = second.Lookup(ctx, value.ResponseID, key.ID, now); err != nil {
				t.Fatal("foreign forget changed ownership")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err = second.Lookup(canceled, value.ResponseID, key.ID, now); !errors.Is(err, context.Canceled) || !errors.Is(err, historydomain.ErrResponseRead) {
				t.Fatal(err)
			}
			if err = second.Forget(canceled, value.ResponseID, key.ID); !errors.Is(err, context.Canceled) || !errors.Is(err, historydomain.ErrResponseDelete) {
				t.Fatal(err)
			}
			if _, err = first.Lookup(ctx, value.ResponseID, key.ID, now); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			results := make(chan error, 12)
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					service := first
					if i%2 != 0 {
						service = second
					}
					results <- service.Forget(ctx, value.ResponseID, key.ID)
				}(i)
			}
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = second.Lookup(ctx, value.ResponseID, key.ID, now); !errors.Is(err, historydomain.ErrResponseNotFound) {
				t.Fatal(err)
			}

			native := inferencedomain.WebResponseState{ResponseID: "native", AccountID: owner.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: `{"id":"native"}`, Status: "completed"}
			if err = first.RecordWeb(ctx, native, now); err != nil {
				t.Fatal(err)
			}
			nativeSaved, err := second.LookupWeb(ctx, "native", now)
			if err != nil {
				t.Fatal(err)
			}
			if !nativeSaved.ExpiresAt.Equal(now.Add(30*24*time.Hour)) || !nativeSaved.CreatedAt.Equal(now) || nativeSaved.ResponseJSON != native.ResponseJSON {
				t.Fatal("native record retention changed")
			}
			invalid := native
			invalid.ResponseID = "incomplete"
			invalid.UpstreamParentResponseID = ""
			if err = first.RecordWeb(ctx, invalid, now); err == nil {
				t.Fatal("incomplete native identity was accepted")
			}
			if _, err = second.LookupWeb(ctx, "incomplete", now); !errors.Is(err, historydomain.ErrResponseNotFound) {
				t.Fatal(err)
			}
			if err = first.DeleteWeb(ctx, "native"); err != nil {
				t.Fatal(err)
			}
			if _, err = second.LookupWeb(ctx, "native", now); !errors.Is(err, historydomain.ErrResponseNotFound) {
				t.Fatal(err)
			}
			// Definite native absence keeps its Provider 404 meaning, while ownership
			// completion acknowledges already-removed rows across instances.
			if err = first.DeleteWeb(ctx, "missing"); !errors.Is(err, historydomain.ErrResponseNotFound) {
				t.Fatal(err)
			}
			if _, err = second.LookupWeb(canceled, "missing", now); !errors.Is(err, context.Canceled) || !errors.Is(err, historydomain.ErrResponseRead) {
				t.Fatal(err)
			}
			if err = second.DeleteWeb(canceled, "missing"); !errors.Is(err, context.Canceled) || !errors.Is(err, historydomain.ErrResponseDelete) {
				t.Fatal(err)
			}
			if _, err = repoA.Get(ctx, value.ResponseID, key.ID, now.UTC()); !errors.Is(err, repository.ErrNotFound) {
				t.Fatal(err)
			}
		})
	}
}
