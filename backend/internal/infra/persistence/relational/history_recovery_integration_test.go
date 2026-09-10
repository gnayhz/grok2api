package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Two independent history services, SQL connections and cache clients race on
// the same persisted generation. Restart must retain only the accepted branch.
func TestHistoryRecoverySharedGenerationIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, cache := range []string{"memory", "redis"} {
			t.Run(dialect+"/"+cache, func(t *testing.T) {
				a, b := settingsDatabasePair(t, dialect)
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					t.Fatal(err)
				}
				prefix := fmt.Sprintf("history-recovery-%d:", time.Now().UnixNano())
				newCache := func() repository.ReasoningReplayRepository {
					if cache == "memory" {
						return memory.NewReasoningReplayStore(32)
					}
					address := os.Getenv("TEST_REDIS_ADDRESS")
					if address == "" {
						t.Skip("TEST_REDIS_ADDRESS is not configured")
					}
					store, err := redisruntime.Open(t.Context(), redisruntime.Config{Address: address, KeyPrefix: prefix})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.Close() })
					return redisruntime.NewReasoningReplayStore(store)
				}
				newService := func(db *Database) *historyapp.ReasoningReplay {
					hot := newCache()
					j := NewConversationJournal(db, cipher, 8<<20).WithHotCache(hot, time.Minute)
					r := historyapp.New(hot, historyapp.Config{Enabled: true, TTL: time.Minute}, nil)
					r.UseJournal(j, time.Hour, time.Hour)
					return r
				}
				services := []*historyapp.ReasoningReplay{newService(a), newService(b)}
				input := []byte(`{"input":[{"role":"user","content":"hello"}]}`)
				_, seed, err := services[0].Prepare(t.Context(), "model", "scope", input)
				if err != nil {
					t.Fatal(err)
				}
				encoded := journalCipherItem(t)
				seedPayload := marshalHistoryOutput(t, "seed", encoded)
				commitHistoryCapture(t, seed, seedPayload)
				next := []byte(`{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`)
				prepared := make([]historydomain.Prepared, 2)
				var steps []historydomain.RecoveryStep
				for i, r := range services {
					restored, p, err := r.Prepare(t.Context(), "model", "scope", next)
					if err != nil {
						t.Fatal(err)
					}
					prepared[i] = p
					defer p.Discard()
					if !strings.Contains(string(restored), encoded["encrypted_content"].(string)) {
						t.Fatal("replica did not restore opaque")
					}
					steps, _ = historyapp.PlanRecovery(historydomain.RecoveryInput{Rejection: historydomain.OpaqueDecodeRejected, Method: "POST", Body: restored, PromptCacheKey: "session"})
				}
				if len(steps) != 2 {
					t.Fatalf("steps=%d", len(steps))
				}
				if err := services[0].ApplyRecovery(t.Context(), nil, "model", "scope", steps[0]); !errors.Is(err, historydomain.ErrHistoryPrepare) {
					t.Fatalf("missing generation err=%v", err)
				}
				if err := services[1].ApplyRecovery(t.Context(), prepared[0], "model", "scope", steps[0]); !errors.Is(err, historydomain.ErrHistoryPrepare) {
					t.Fatalf("foreign prepared turn err=%v", err)
				}

				// Cancellation before the state transition leaves both writers current.
				canceled, cancel := context.WithCancel(t.Context())
				cancel()
				if err := services[0].ApplyRecovery(canceled, prepared[0], "model", "scope", steps[0]); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel err=%v", err)
				}
				gate := make(chan struct{})
				results := make(chan error, 2)
				var wg sync.WaitGroup
				for i := range services {
					wg.Go(func() {
						<-gate
						results <- services[i].ApplyRecovery(t.Context(), prepared[i], "model", "scope", steps[0])
					})
				}
				close(gate)
				wg.Wait()
				close(results)
				accepted, stale := 0, 0
				for err := range results {
					if err == nil {
						accepted++
					} else if errors.Is(err, historydomain.ErrHistoryStale) {
						stale++
					} else {
						t.Fatal(err)
					}
				}
				if accepted != 1 || stale != 1 {
					t.Fatalf("accepted=%d stale=%d", accepted, stale)
				}
				// Both old writers must be unable to commit a delayed answer after reset.
				for _, p := range prepared {
					capture, commit, discard := p.Capture(io.NopCloser(strings.NewReader(seedPayload)), false)
					_, _ = io.Copy(io.Discard, capture)
					_ = capture.Close()
					if err = commit(); !errors.Is(err, historydomain.ErrHistoryStale) {
						t.Fatalf("stale writer accepted: %v", err)
					}
					discard()
				}
				_, current, err := services[1].Prepare(t.Context(), "model", "scope", steps[0].Body)
				if err != nil {
					t.Fatal(err)
				}
				if current.Generation() != 2 {
					t.Fatalf("generation=%d", current.Generation())
				}
				newOpaque := journalCipherItem(t)
				commitHistoryCapture(t, current, marshalHistoryOutput(t, "winner", newOpaque))
				restarted := newService(a)
				body := []byte(`{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"},{"role":"assistant","content":"answer"},{"role":"user","content":"again"}]}`)
				restored, last, err := restarted.Prepare(t.Context(), "model", "scope", body)
				if err != nil {
					t.Fatal(err)
				}
				defer last.Discard()
				if last.Generation() != 2 || !strings.Contains(string(restored), newOpaque["encrypted_content"].(string)) || strings.Contains(string(restored), encoded["encrypted_content"].(string)) {
					t.Fatal("restart restored old generation or lost winner")
				}
			})
		}
	}
}
func marshalHistoryOutput(t *testing.T, id string, opaque map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"id": id, "status": "completed", "output": []any{opaque, map[string]any{"role": "assistant", "content": "answer"}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func commitHistoryCapture(t *testing.T, p historydomain.Prepared, payload string) {
	t.Helper()
	capture, commit, discard := p.Capture(io.NopCloser(strings.NewReader(payload)), false)
	defer discard()
	_, err := io.Copy(io.Discard, capture)
	_ = capture.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = commit(); err != nil {
		t.Fatal(err)
	}
}
