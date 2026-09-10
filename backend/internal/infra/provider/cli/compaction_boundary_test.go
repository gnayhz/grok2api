package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gatewayapp "github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestCompactionInputBoundaryHTTP(t *testing.T) {
	for _, mode := range []string{"foreign_no_controller", "foreign_preserve", "owned_precision", "owned_escaped", "foreign_allowed", "damaged_preserve", "damaged_allowed", "non_string_preserve"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			wire := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, _ := io.ReadAll(r.Body)
				wire <- string(data)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_compaction_boundary","object":"response","status":"completed","model":"grok-4.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
			}))
			defer server.Close()
			adapter, encrypted := newCompactionTestAdapter(t)
			cfg := adapter.config()
			cfg.BaseURL = server.URL
			cfg.FallbackBaseURL = ""
			adapter.UpdateConfig(cfg)
			blob := "foreign-private-state"
			if strings.HasPrefix(mode, "damaged") {
				blob = "g2a_compact_v1.invalid"
			}
			if strings.HasPrefix(mode, "owned") {
				var err error
				blob, err = adapter.compaction.Encode("session", "SYNTHETIC_SUMMARY_PRESERVED")
				if err != nil {
					t.Fatal(err)
				}
			}
			itemType := `compaction`
			if mode == "owned_escaped" {
				itemType = `\u0063ompaction`
			}
			body := []byte(`{"input":[{"type":"` + itemType + `","encrypted_content":` + mustJSONString(blob) + `},{"role":"user","content":"continue"}],"tools":[{"type":"function","name":"lookup","parameters":` + precisionSchema + `}],"tool_choice":"none"}`)
			if mode == "non_string_preserve" {
				body = []byte(`{"input":[{"type":"compaction","encrypted_content":17},{"role":"user","content":"continue"}]}`)
			}
			req := provider.ResponseResourceRequest{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.5", Operation: "responses", PromptCacheKey: "session", Body: body, NormalizeBody: true}
			if strings.HasSuffix(mode, "preserve") {
				req.HistoryControl = gatewayapp.NewHistoryController(historydomain.PreserveOpaque, inferencedomain.NewAttemptBudget(3))
			}
			if strings.HasSuffix(mode, "allowed") {
				req.HistoryControl = gatewayapp.NewHistoryController(historydomain.AllowLossyRecovery, inferencedomain.NewAttemptBudget(3))
			}
			response, err := adapter.ForwardResponse(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if mode == "foreign_no_controller" || strings.HasSuffix(mode, "preserve") {
				if calls.Load() != 0 || response.StatusCode != 400 || response.RequestValidation == nil {
					t.Fatalf("unapproved compaction loss: physical=%d status=%d localValidation=%v", calls.Load(), response.StatusCode, response.RequestValidation)
				}
				return
			}
			if calls.Load() != 1 {
				t.Fatalf("physical=%d status=%d", calls.Load(), response.StatusCode)
			}
			data := <-wire
			if strings.HasSuffix(mode, "allowed") {
				if !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "foreign_compaction_omitted") {
					t.Fatal("missing loss warning")
				}
				if strings.Contains(data, blob) || !strings.Contains(data, "retained conversation messages") {
					t.Fatalf("loss boundary missing: %s", data)
				}
			} else if !strings.Contains(data, "SYNTHETIC_SUMMARY_PRESERVED") {
				t.Errorf("equivalent compaction spelling lost summary: %s", data)
			}
			for _, number := range []string{"9007199254740993", "0.1234567890123456789"} {
				if !strings.Contains(data, number) {
					t.Errorf("compaction mutated unrelated schema number %s: %s", number, data)
				}
			}
		})
	}
}

func TestCompactionSamplingPreservesSchemaNumbers(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"summarize"}],"tools":[{"type":"function","name":"lookup","parameters":` + precisionSchema + `}]}`)
	sample, err := prepareGatewayCompactionSample(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{"9007199254740993", "0.1234567890123456789"} {
		if !strings.Contains(string(sample), number) {
			t.Fatalf("summary sample changed %s: %s", number, sample)
		}
	}
}

type compactionDecodeGate struct {
	historydomain.CompactionCipher
	entered chan struct{}
	release chan struct{}
}

func (c compactionDecodeGate) Decrypt(value string) (string, error) {
	if value == "synthetic-invalid" {
		close(c.entered)
		<-c.release
	}
	return c.CompactionCipher.Decrypt(value)
}

func TestCompactionPreparationDoesNotResetJournalOnDenialFailureOrCancellation(t *testing.T) {
	for _, scenario := range []string{"strict", "failure", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":"invalid synthetic request"}`)
			}))
			defer upstream.Close()
			adapter, replay, request := newRecoveryJournalAdapter(t, upstream.URL, true)
			request.Body = []byte(`{"input":[{"type":"compaction","encrypted_content":"g2a_compact_v1.synthetic-invalid"},{"role":"user","content":"next"}]}`)
			mode := historydomain.AllowLossyRecovery
			if scenario == "strict" {
				mode = historydomain.PreserveOpaque
			}
			request.HistoryControl = gatewayapp.NewHistoryController(mode, inferencedomain.NewAttemptBudget(2))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			if scenario == "canceled" {
				adapter.compaction = historydomain.NewCompactionCodec(compactionDecodeGate{CompactionCipher: adapter.cipher, entered: entered, release: release})
			}
			done := make(chan error, 1)
			go func() {
				res, err := adapter.ForwardResponse(ctx, request)
				if res != nil {
					_, _ = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
					if res.DiscardOutput != nil {
						res.DiscardOutput()
					}
				}
				done <- err
			}()
			if scenario == "canceled" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					close(release)
					t.Fatal("decode gate not reached")
				}
				cancel()
				close(release)
			}
			select {
			case err := <-done:
				if scenario == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel err=%v", err)
				}
				if scenario != "canceled" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("compaction prepare did not finish")
			}
			wantCalls := int64(0)
			if scenario == "failure" {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("calls=%d want=%d", calls.Load(), wantCalls)
			}
			key := adapter.scopedReasoningReplayKey(request, upstream.URL+"/v1")
			input, prepared, err := replay.Prepare(context.Background(), request.Model, key, recoveryInput("responses", false))
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Discard()
			if prepared.Generation() != 1 || !strings.Contains(string(input), recoveryOpaque(0)) {
				t.Fatal("local compaction preparation invalidated committed history")
			}
		})
	}
}
