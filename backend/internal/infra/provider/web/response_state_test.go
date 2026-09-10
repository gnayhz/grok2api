package web

import (
	"context"
	"errors"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestWebResponseStateWaitsForCompletionAndReleasesResources(t *testing.T) {
	for _, stage := range []string{"commit", "discard", "cancel_write", "close_early", "missing_identity"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "states.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			credential, _, err := relational.NewAccountRepository(db).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "state-test", SourceKey: "state-test", EncryptedAccessToken: "test-token", AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			states := relational.NewResponseRepository(db)
			pool := responsebuffer.NewPool(8 << 20)
			ctx = responsebuffer.WithContext(ctx, pool.Request(4<<20))
			adapter := &Adapter{states: historyapp.NewResponseResources(states)}
			pending := adapter.pendingResponseState(ctx, conversation.OperationResponses, nil)
			raw := `{"result":{"conversation":{"conversationId":"conversation"}}}` + "\n" + `{"result":{"response":{"userResponse":{"responseId":"parent"},"token":"answer","messageTag":"final"}}}`
			if stage == "missing_identity" {
				raw = `{"result":{"response":{"token":"answer","messageTag":"final"}}}`
			}
			source := &closedWebSource{Reader: strings.NewReader(raw), done: make(chan struct{})}
			body := adapter.streamOpenAIResponse(ctx, source, new(egress.Lease), credential, "resp_state", "grok", conversation.OperationResponses, "prompt", nil, toolConfiguration{}, true, conversation.ResponseOptions{}, pending, "")
			response := pending.attach(&provider.Response{Body: body})
			if stage == "close_early" {
				_, _ = io.CopyN(io.Discard, body, 10)
			} else if _, err := io.Copy(io.Discard, body); err != nil {
				t.Fatal(err)
			}
			_ = body.Close()
			select {
			case <-source.done:
			case <-time.After(2 * time.Second):
				t.Fatal("conversion retained source after close")
			}
			if _, err := states.GetWebState(ctx, "resp_state", time.Now()); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("reading or closing committed native state: %v", err)
			}
			if stage != "close_early" && pool.Snapshot().Used == 0 {
				t.Fatal("pending state bypassed resource budget")
			}
			switch stage {
			case "commit":
				if err := response.CommitResponseState(ctx); err != nil {
					t.Fatal(err)
				}
				if err := response.CommitResponseState(ctx); err != nil {
					t.Fatalf("idempotent receipt: %v", err)
				}
			case "cancel_write":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				if err := response.CommitResponseState(canceled); !errors.Is(err, context.Canceled) {
					t.Fatalf("missing write failure: %v", err)
				}
				if err := response.CommitResponseState(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("failed receipt silently retried: %v", err)
				}
			case "missing_identity":
				if err := response.CommitResponseState(ctx); err == nil {
					t.Fatal("accepted incomplete continuation identity")
				}
			}
			response.DiscardOutput()
			response.DiscardOutput()
			if used := pool.Snapshot().Used; used != 0 {
				t.Fatalf("completion retained %d bytes", used)
			}
			got, err := states.GetWebState(ctx, "resp_state", time.Now())
			if stage == "commit" {
				if err != nil || got.ConversationID != "conversation" || got.UpstreamParentResponseID != "parent" || !strings.Contains(got.ResponseJSON, "answer") {
					t.Fatalf("discard erased durable state: %+v %v", got, err)
				}
			} else if !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("failed/discarded state persisted: %+v %v", got, err)
			}
		})
	}
}
