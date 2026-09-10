package relational

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func identityJournalFixture(t *testing.T, dialect string) (*Database, *ConversationJournal) {
	t.Helper()
	if dialect == "sqlite" {
		db, j, _ := journalFixture(t)
		return db, j
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated TEST_POSTGRES_DSN required")
	}
	db, err := OpenPostgres(context.Background(), dsn, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.db.AutoMigrate(&conversationSessionModel{}, &conversationRequestModel{}, &conversationTurnModel{}); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewVersionedCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", nil)
	if err != nil {
		t.Fatal(err)
	}
	return db, NewConversationJournal(db, cipher, 8<<20)
}
func commitIdentityTurn(t *testing.T, p historydomain.Prepared, id, answer string, opaque map[string]any) {
	t.Helper()
	output := []any{opaque, map[string]any{"type": "message", "role": "assistant", "content": answer}}
	payload, err := json.Marshal(map[string]any{"id": id, "status": "completed", "output": output})
	if err != nil {
		t.Fatal(err)
	}
	stream, commit, discard := p.Capture(io.NopCloser(bytes.NewReader(payload)), false)
	defer discard()
	if _, err := io.Copy(io.Discard, stream); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
}
func identityBody(t *testing.T, items []any) []byte {
	t.Helper()
	b, e := json.Marshal(map[string]any{"input": items})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func identityMessage(role, text string) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": text}
}

type identityInspectionFailure struct {
	repository.ConversationJournal
	err error
}

func (j identityInspectionFailure) Inspect(context.Context, []repository.JournalScope, time.Time) ([]repository.JournalSnapshot, error) {
	return nil, j.err
}

func TestConversationIdentityMigrationChecksLossWithoutImportingOldScope(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, legacyHash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy_hash=%t", dialect, legacyHash), func(t *testing.T) {
				ctx := context.Background()
				db, j := identityJournalFixture(t, dialect)
				suffix := fmt.Sprint(time.Now().UnixNano())
				oldKey := "old-identity-" + suffix
				oldScope := repository.JournalScope{Key: oldKey, Model: "model", Normalizer: 1}
				oldID := journalScopeID(oldScope)
				firstCipher, secondCipher := journalCipherItem(t), journalCipherItem(t)
				old := newJournalReplay(j)
				firstInput := []any{identityMessage("user", "one")}
				_, p, err := old.Prepare(ctx, "model", oldKey, identityBody(t, firstInput))
				if err != nil {
					t.Fatal(err)
				}
				commitIdentityTurn(t, p, "old-first-"+suffix, "first", firstCipher)
				secondInput := append(firstInput, identityMessage("assistant", "first"), identityMessage("user", "two"))
				_, p, err = old.Prepare(ctx, "model", oldKey, identityBody(t, secondInput))
				if err != nil {
					t.Fatal(err)
				}
				commitIdentityTurn(t, p, "old-second-"+suffix, "second", secondCipher)
				if legacyHash {

					// Pre-visible-hash roots stored full input and lacked parent links.
					var last conversationTurnModel
					if err := db.db.Where("session = ? AND response_id = ?", oldID, "old-second-"+suffix).First(&last).Error; err != nil {
						t.Fatal(err)
					}
					var full [][]byte
					for _, item := range secondInput {
						raw, e := json.Marshal(item)
						if e != nil {
							t.Fatal(e)
						}
						full = append(full, raw)
					}
					encrypted, e := j.encrypt(full)
					if e != nil {
						t.Fatal(e)
					}
					if err := db.db.Model(&last).Updates(map[string]any{"parent": "", "encrypted_input": encrypted}).Error; err != nil {
						t.Fatal(err)
					}
					if err := db.db.Model(&conversationSessionModel{}).Where("id = ?", oldID).Update("visible_hash_version", 0).Error; err != nil {
						t.Fatal(err)
					}
					if err := db.db.Model(&conversationTurnModel{}).Where("session = ?", oldID).Update("prefix_hash", "legacy-config-hash").Error; err != nil {
						t.Fatal(err)
					}
				}
				// Capture every durable column of the old identity, including expiry and
				// migration markers. Preparing/discarding new writers must change none.
				snapshot := func() any {
					var session conversationSessionModel
					var turns []conversationTurnModel
					var requests []conversationRequestModel
					if e := db.db.First(&session, "id = ?", oldID).Error; e != nil {
						t.Fatal(e)
					}
					if e := db.db.Where("session = ?", oldID).Order("id").Find(&turns).Error; e != nil {
						t.Fatal(e)
					}
					if e := db.db.Where("session = ?", oldID).Order("id").Find(&requests).Error; e != nil {
						t.Fatal(e)
					}
					return struct {
						Session  conversationSessionModel
						Turns    []conversationTurnModel
						Requests []conversationRequestModel
					}{session, turns, requests}
				}
				before := snapshot()
				for _, carry := range []string{"none", "partial", "partial_latest", "complete", "conflict"} {
					for _, mode := range []historydomain.RecoveryMode{historydomain.PreserveOpaque, historydomain.AllowLossyRecovery} {
						t.Run(fmt.Sprintf("%s/mode=%d", carry, mode), func(t *testing.T) {
							input := []any{identityMessage("user", "one")}
							if carry == "partial" || carry == "complete" {
								input = append(input, firstCipher)
							}
							if carry == "conflict" {
								input = append(input, journalCipherItem(t))
							}
							input = append(input, identityMessage("assistant", "first"), identityMessage("user", "two"))
							if carry == "complete" || carry == "partial_latest" {
								input = append(input, secondCipher)
							}
							input = append(input, identityMessage("assistant", "second"), identityMessage("user", "next"))
							body := identityBody(t, input)
							newKey := fmt.Sprintf("new-%s-%d-%s", carry, mode, suffix)
							budget := inferencedomain.NewAttemptBudget(1)
							defer budget.Close()
							option := historydomain.ReplayPreparation{PriorKeys: []string{oldKey}, Authorizer: gateway.NewHistoryController(mode, budget)}
							replay := newJournalReplay(j)
							got, prepared, e := replay.Prepare(ctx, "model", newKey, body, option)
							denied := mode == historydomain.PreserveOpaque && carry != "complete"
							if denied {
								if !errors.Is(e, historydomain.ErrIdentityLossNotAuthorized) || prepared != nil {
									t.Fatalf("strict result prepared=%v err=%v", prepared, e)
								}
								var active int64
								if e := db.db.Model(&conversationRequestModel{}).Where("session = ?", journalScopeID(repository.JournalScope{Key: newKey, Model: "model", Normalizer: 1})).Count(&active).Error; e != nil || active != 0 {
									t.Fatalf("denied writer retained: active=%d err=%v", active, e)
								}
							} else {
								if e != nil {
									t.Fatal(e)
								}
								if !bytes.Equal(got, body) || prepared.RestoredItems() != 0 {
									t.Fatal("ambiguous old opaque was imported")
								}
								acceptedCipher := journalCipherItem(t)
								commitIdentityTurn(t, prepared, newKey, "accepted", acceptedCipher)
								// A committed new branch is authoritative after restart; strict recovery
								// must not repeatedly inspect the inaccessible old branch.
								broken := identityInspectionFailure{ConversationJournal: j, err: errors.New("old inspection must not run")}
								restarted := newJournalReplay(j)
								restarted.UseJournal(broken, time.Hour, time.Hour)
								input = append(input, identityMessage("assistant", "accepted"), identityMessage("user", "after restart"))
								strict := historydomain.ReplayPreparation{PriorKeys: []string{oldKey}, Authorizer: gateway.NewHistoryController(historydomain.PreserveOpaque, budget)}
								got, next, e := restarted.Prepare(ctx, "model", newKey, identityBody(t, input), strict)
								if e != nil {
									t.Fatal(e)
								}
								defer next.Discard()
								if !bytes.Contains(got, []byte(acceptedCipher["encrypted_content"].(string))) {
									t.Fatal("new branch did not restore after restart")
								}
							}
							if !reflect.DeepEqual(before, snapshot()) {
								t.Fatal("old scope changed during identity separation")
							}
						})
					}
				}
				// Neither nil authorization nor a storage failure may silently permit loss.
				visible := append(secondInput, identityMessage("assistant", "second"), identityMessage("user", "next"))
				body := identityBody(t, visible)
				_, p, err = old.Prepare(ctx, "model", "absent-policy-"+suffix, body, historydomain.ReplayPreparation{PriorKeys: []string{oldKey}})
				if !errors.Is(err, historydomain.ErrIdentityLossNotAuthorized) || p != nil {
					t.Fatalf("absent policy err=%v", err)
				}
				broken := identityInspectionFailure{ConversationJournal: j, err: errors.New("inspection unavailable")}
				fail := newJournalReplay(j)
				fail.UseJournal(broken, time.Hour, time.Hour)
				_, p, err = fail.Prepare(ctx, "model", "failed-inspection-"+suffix, body, historydomain.ReplayPreparation{PriorKeys: []string{oldKey}})
				if err == nil || !errors.Is(err, historydomain.ErrHistoryPrepare) || p != nil {
					t.Fatalf("inspection error lost: %v", err)
				}
				if !reflect.DeepEqual(before, snapshot()) {
					t.Fatal("failed preparation changed old scope")
				}
			})
		}
	}
}

// Keep the compile-time implementation contract explicit for test wrappers.
var _ historydomain.Service = (*historyapp.ReasoningReplay)(nil)

func TestIdentityLegacyCacheKeepsLossAuthorization(t *testing.T) {
	outputMessage := func(text string) map[string]any {
		return map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
	}
	ctx := context.Background()
	for _, carry := range []bool{false, true} {
		t.Run(fmt.Sprint(carry), func(t *testing.T) {
			cache := memory.NewReasoningReplayStore(20)
			replay := historyapp.New(cache, historyapp.Config{Enabled: true, TTL: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			opaque := journalCipherItem(t)
			payload, _ := json.Marshal(map[string]any{"status": "completed", "output": []any{opaque, outputMessage("old answer")}})
			replay.StoreFromCompleted(ctx, "model", "old-cache", payload)
			input := []any{identityMessage("user", "one")}
			if carry {
				input = append(input, opaque)
			}
			input = append(input, identityMessage("assistant", "old answer"), identityMessage("user", "next"))
			body := identityBody(t, input)
			budget := inferencedomain.NewAttemptBudget(1)
			defer budget.Close()
			option := historydomain.ReplayPreparation{PriorKeys: []string{"old-cache"}, Authorizer: gateway.NewHistoryController(historydomain.PreserveOpaque, budget)}
			got, p, err := replay.Prepare(ctx, "model", "new-cache", body, option)
			if p != nil {
				t.Fatal("cache-only history allocated durable writer")
			}
			if !carry && !errors.Is(err, historydomain.ErrIdentityLossNotAuthorized) {
				t.Fatalf("strict cache error=%v", err)
			}
			if carry && (err != nil || !bytes.Equal(got, body)) {
				t.Fatalf("client carried history changed: %v", err)
			}
			option.Authorizer = gateway.NewHistoryController(historydomain.AllowLossyRecovery, budget)
			got, _, err = replay.Prepare(ctx, "model", "new-cache", body, option)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("allowed cache migration imported old data: %v", err)
			}
			accepted := journalCipherItem(t)
			payload, _ = json.Marshal(map[string]any{"status": "completed", "output": []any{accepted, outputMessage("accepted")}})
			replay.StoreFromCompleted(ctx, "model", "new-cache", payload)
			input = append(input, identityMessage("assistant", "accepted"), identityMessage("user", "again"))
			option.Authorizer = gateway.NewHistoryController(historydomain.PreserveOpaque, budget)
			got, _, err = replay.Prepare(ctx, "model", "new-cache", identityBody(t, input), option)
			if err != nil || !bytes.Contains(got, []byte(accepted["encrypted_content"].(string))) {
				t.Fatalf("current cache did not become authoritative: %v", err)
			}
			original, found, err := cache.Get(ctx, "model", "old-cache", time.Now().UTC(), time.Hour)
			if err != nil || !found || len(original) == 0 {
				t.Fatal("old cache was cleared")
			}
		})
	}
}

func TestConversationIdentityInspectionUTCAndCancellation(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			_, journal := identityJournalFixture(t, dialect)
			replay := newJournalReplay(journal)
			key := fmt.Sprintf("inspect-clock-%d", time.Now().UnixNano())
			_, prepared, err := replay.Prepare(context.Background(), "model", key, identityBody(t, []any{identityMessage("user", "hello")}))
			if err != nil {
				t.Fatal(err)
			}
			commitIdentityTurn(t, prepared, "inspect-response-"+key, "answer", journalCipherItem(t))
			scopes := []repository.JournalScope{{Key: key, Model: "model", Normalizer: 1}}
			now := time.Now().UTC()
			utc, err := journal.Inspect(context.Background(), scopes, now)
			if err != nil {
				t.Fatal(err)
			}
			local, err := journal.Inspect(context.Background(), scopes, now.In(time.FixedZone("east", 8*3600)))
			if err != nil {
				t.Fatal(err)
			}
			if len(utc) != 1 || !reflect.DeepEqual(utc, local) {
				t.Fatal("time zone changed inspection visibility")
			}
			expired, err := journal.Inspect(context.Background(), scopes, now.Add(48*time.Hour))
			if err != nil || len(expired) != 0 {
				t.Fatalf("expired snapshot=%v err=%v", expired, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = journal.Inspect(ctx, scopes, now)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("inspection ignored cancellation: %v", err)
			}
		})
	}
}

func TestConversationIdentityConcurrentWritersDoNotFenceOldScope(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			db, journal := identityJournalFixture(t, dialect)
			replay := newJournalReplay(journal)
			suffix := fmt.Sprint(time.Now().UnixNano())
			oldKey := "old-concurrent-" + suffix
			oldCipher := journalCipherItem(t)
			opening := []any{identityMessage("user", "opening")}
			_, prepared, err := replay.Prepare(ctx, "model", oldKey, identityBody(t, opening))
			if err != nil {
				t.Fatal(err)
			}
			commitIdentityTurn(t, prepared, "old-root-"+suffix, "old answer", oldCipher)
			prefix := append(opening, identityMessage("assistant", "old answer"))
			_, oldWriter, err := replay.Prepare(ctx, "model", oldKey, identityBody(t, append(prefix, identityMessage("user", "old writer"))))
			if err != nil {
				t.Fatal(err)
			}
			defer oldWriter.Discard()
			const writers = 8
			inputs := make([][]byte, writers)
			outputs := make([]map[string]any, writers)
			for i := range writers {
				inputs[i] = identityBody(t, append(prefix, identityMessage("user", fmt.Sprintf("new-%d", i))))
				outputs[i] = journalCipherItem(t)
			}
			var wg sync.WaitGroup
			errorsOut := make(chan error, writers)
			for i := range writers {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					budget := inferencedomain.NewAttemptBudget(1)
					defer budget.Close()
					option := historydomain.ReplayPreparation{PriorKeys: []string{oldKey}, Authorizer: gateway.NewHistoryController(historydomain.AllowLossyRecovery, budget)}
					current := newJournalReplay(journal)
					body, p, e := current.Prepare(ctx, "model", fmt.Sprintf("new-%d-%s", i%2, suffix), inputs[i], option)
					if e != nil {
						errorsOut <- e
						return
					}
					defer p.Discard()
					if bytes.Contains(body, []byte(oldCipher["encrypted_content"].(string))) {
						errorsOut <- errors.New("concurrent writer imported old opaque")
						return
					}
					output, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("new-response-%d-%s", i, suffix), "status": "completed", "output": []any{outputs[i], identityMessage("assistant", fmt.Sprintf("answer-%d", i))}})
					stream, commit, discard := p.Capture(io.NopCloser(bytes.NewReader(output)), false)
					defer discard()
					_, e = io.Copy(io.Discard, stream)
					_ = stream.Close()
					if e == nil {
						e = commit()
					}
					errorsOut <- e
				}(i)
			}
			wg.Wait()
			close(errorsOut)
			for err := range errorsOut {
				if err != nil {
					t.Fatal(err)
				}
			}
			// A request already accepted by the old instance retains its original ticket.
			commitIdentityTurn(t, oldWriter, "old-late-"+suffix, "old accepted", journalCipherItem(t))
			for _, entry := range []struct {
				key   string
				count int64
			}{{oldKey, 2}, {"new-0-" + suffix, writers / 2}, {"new-1-" + suffix, writers / 2}} {
				scope := journalScopeID(repository.JournalScope{Key: entry.key, Model: "model", Normalizer: 1})
				var count int64
				if err := db.db.Model(&conversationTurnModel{}).Where("session = ?", scope).Count(&count).Error; err != nil || count != entry.count {
					t.Fatalf("turn count=%d want=%d err=%v", count, entry.count, err)
				}
			}
		})
	}
}
