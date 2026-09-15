package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	reasoningreplay "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestPersistentHistorySurvivesClientConversion(t *testing.T) {
	for _, op := range []string{"responses", "chat", "messages"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", op, streaming), func(t *testing.T) {
				ctx := context.Background()
				db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "history.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if err = db.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
				cipher, err := security.NewVersionedCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)), nil)
				if err != nil {
					t.Fatal(err)
				}
				token, err := cipher.Encrypt("access-token")
				if err != nil {
					t.Fatal(err)
				}
				journal := relational.NewConversationJournal(db, cipher, 64<<20)
				replay := reasoningreplay.New(memory.NewReasoningReplayStore(10), reasoningreplay.Config{Enabled: true, TTL: time.Hour}, nil)
				replay.UseJournal(journal, 24*time.Hour, time.Hour)
				adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
				adapter.SetReasoningReplay(replay)
				raw := make([]byte, 32)
				for i := range raw {
					raw[i] = byte(i)
				}
				enc := base64.RawStdEncoding.EncodeToString(raw)
				payload := `{"id":"r1","status":"completed","output":[{"type":"reasoning","encrypted_content":"` + enc + `"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
				if streaming {
					payload = "data: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\n"
				}
				var sent []string
				adapter.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					data, _ := io.ReadAll(req.Body)
					sent = append(sent, string(data))
					return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
				})
				request := provider.ResponseResourceRequest{Credential: account.Credential{ID: 7, Provider: account.ProviderBuild, EncryptedAccessToken: token}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.6", Operation: op, ReasoningReplayKey: "persistent-session", Body: []byte(`{"input":"hello"}`), Streaming: streaming, DeferOutputCommit: true}
				response, err := adapter.ForwardResponse(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				if response.CommitOutput == nil {
					t.Fatal("missing durable callback")
				}
				body := response.Body
				if streaming && response.ConvertStream != nil {
					body = response.ConvertStream(body)
				}
				data, err := io.ReadAll(body)
				if err != nil {
					t.Fatal(err)
				}
				_ = body.Close()
				if !streaming && response.ConvertJSON != nil {
					if _, err = response.ConvertJSON(data); err != nil {
						t.Fatal(err)
					}
				}
				if err = response.CommitOutput(); err != nil {
					t.Fatal(err)
				}
				response.DiscardOutput()
				request.Body = []byte(`{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`)
				next, err := adapter.ForwardResponse(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				_ = next.Body.Close()
				next.DiscardOutput()
				if len(sent) != 2 || !strings.Contains(sent[1], enc) {
					t.Fatal("durable reasoning missing after conversion")
				}
			})
		}
	}
}
