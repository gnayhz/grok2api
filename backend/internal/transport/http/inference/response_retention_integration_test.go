package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Each preference reaches auth, model/account selection, the real provider
// HTTP/WS wire, durable completion and resource routes in both SQL dialects.
func TestHTTPResponseStorageContract(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []account.Provider{account.ProviderWeb, account.ProviderBuild, account.ProviderConsole} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", dialect, kind, streaming), func(t *testing.T) {
					f := newResponseRetentionFixture(t, dialect, kind)
					for _, preference := range []struct {
						name   string
						fields map[string]any
						stores bool
					}{
						{"false", map[string]any{"store": false}, false},
						{"true", map[string]any{"store": true}, true},
						{"omitted", nil, true},
						{"null", map[string]any{"store": nil}, true},
					} {
						t.Run(preference.name, func(t *testing.T) {
							before, beforeOwner, beforeState, beforeHistory := f.generated.Load(), f.states.ownershipCalls.Load(), f.states.stateCalls.Load(), f.journal.calls.Load()
							output := f.create(t, streaming, preference.fields, "retention-"+preference.name)
							value := retentionResponse(t, output, streaming)
							id := value["id"].(string)
							record := f.lastAudit(t)
							if f.generated.Load() != before+1 || record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" || record.ErrorCode != "" || record.InputTokens <= 0 || record.OutputTokens <= 0 || record.DeliveredBytes != int64(output.Body.Len()) {
								t.Fatalf("generation or accounting changed: calls=%d record=%+v body=%s", f.generated.Load()-before, record, output.Body.String())
							}
							if len(record.GenerationUsages) != 1 || !record.GenerationUsages[0].Selected || record.GenerationUsages[0].AccountID != f.accountID || record.PhysicalReceipt != "committed" {
								t.Fatalf("lost physical generation: %+v", record)
							}
							stores := preference.stores && kind != account.ProviderConsole
							wantOwner, wantState, wantHistory := "not_required", "not_required", "not_required"
							ownerCalls, stateCalls, historyCalls := int32(0), int32(0), int32(0)
							if stores {
								wantOwner, ownerCalls = "committed", 1
							}
							if stores && kind == account.ProviderWeb {
								wantState, stateCalls = "committed", 1
							}
							if kind == account.ProviderBuild {
								wantHistory, historyCalls = "committed", 1
							}
							if record.OwnershipCommit != wantOwner || record.ProviderStateCommit != wantState || record.HistoryCommit != wantHistory || f.states.ownershipCalls.Load()-beforeOwner != ownerCalls || f.states.stateCalls.Load()-beforeState != stateCalls || f.journal.calls.Load()-beforeHistory != historyCalls {
								t.Fatalf("wrong independent storage outcomes: %+v", record)
							}
							if kind == account.ProviderWeb {
								if value["store"] != preference.stores {
									t.Errorf("store=%v wanted=%t", value["store"], preference.stores)
								}
								if streaming {
									for _, line := range strings.Split(output.Body.String(), "\n") {
										var event map[string]any
										if strings.HasPrefix(line, "data: ") && json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "response.created" {
											if event["response"].(map[string]any)["store"] != preference.stores {
												t.Errorf("created storage disagrees: %s", line)
											}
										}
									}
								}
							} else {
								f.mu.Lock()
								wireStore := f.wireStores[len(f.wireStores)-1]
								f.mu.Unlock()
								wantWire := preference.name == "true" && kind == account.ProviderBuild
								if wireStore != wantWire {
									t.Errorf("wire store=%v want=%t", wireStore, wantWire)
								}
							}
							if !stores {
								f.assertMissing(t, id)
								if kind != account.ProviderConsole {
									denied := f.create(t, streaming, map[string]any{"previous_response_id": id}, "unavailable-parent")
									if denied.Code != 404 || f.generated.Load() != before+1 {
										t.Errorf("temporary parent reused: status=%d calls=%d body=%s", denied.Code, f.generated.Load()-before, denied.Body.String())
									}
								}
							} else {
								if _, err := f.states.Get(context.Background(), id, f.keyID, time.Now()); err != nil {
									t.Fatal(err)
								}
								if kind == account.ProviderWeb {
									got := f.request(http.MethodGet, "/v1/responses/"+id, nil, "")
									if got.Code != 200 || !strings.Contains(got.Body.String(), "completion answer") {
										t.Fatalf("saved GET=%d: %s", got.Code, got.Body.String())
									}
									removed := f.request(http.MethodDelete, "/v1/responses/"+id, nil, "")
									if removed.Code != 200 {
										t.Fatalf("saved DELETE=%d: %s", removed.Code, removed.Body.String())
									}
									f.assertMissing(t, id)
								}
							}
						})
					}
				})
			}
		}
	}
}

func TestHTTPResponseStorageFailureBarrier(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, streaming := range []bool{false, true} {
			for _, stage := range []string{"state", "ownership"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", dialect, streaming, stage), func(t *testing.T) {
					f := newResponseRetentionFixture(t, dialect, account.ProviderWeb)
					f.states.failState, f.states.failOwnership = stage == "state", stage == "ownership"
					output := f.create(t, streaming, map[string]any{"store": false}, "not-stored")
					value := retentionResponse(t, output, streaming)
					f.assertMissing(t, value["id"].(string))
					if f.states.stateCalls.Load() != 0 || f.states.ownershipCalls.Load() != 0 {
						t.Fatal("disabled resource attempted persistence")
					}
					record := f.lastAudit(t)
					if record.ProviderStateCommit != "not_required" || record.OwnershipCommit != "not_required" || record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" {
						t.Fatalf("false affected completion: %+v", record)
					}
					failed := f.create(t, streaming, map[string]any{"store": true}, "stored")
					record = f.lastAudit(t)
					code := "provider_state_commit_failed"
					if stage == "ownership" {
						code = "response_ownership_commit_failed"
					}
					if !strings.Contains(failed.Body.String(), code) || strings.Contains(failed.Body.String(), `"type":"response.completed"`) || record.ErrorCode != code || record.GenerationOutcome != "completed" || record.DeliveryOutcome == "completed" || record.LedgerOutcome != "committed" || f.generated.Load() != 2 {
						t.Fatalf("stored barrier bypassed: record=%+v calls=%d status=%d body=%s", record, f.generated.Load(), failed.Code, failed.Body.String())
					}
					if !streaming && failed.Code != 502 {
						t.Errorf("JSON status=%d", failed.Code)
					}
				})
			}
		}
	}
}

func TestHTTPResponseStorageFalseCanUseStoredParent(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", dialect, streaming), func(t *testing.T) {
				f := newResponseRetentionFixture(t, dialect, account.ProviderWeb)
				parent := retentionResponse(t, f.create(t, streaming, map[string]any{"store": true}, "parent"), streaming)["id"].(string)
				child := retentionResponse(t, f.create(t, streaming, map[string]any{"store": false, "previous_response_id": parent}, "child"), streaming)["id"].(string)
				f.assertMissing(t, child)
				got := f.request(http.MethodGet, "/v1/responses/"+parent, nil, "")
				if got.Code != 200 || f.generated.Load() != 2 || f.states.stateCalls.Load() != 1 || f.states.ownershipCalls.Load() != 1 {
					t.Fatalf("parent lost or child persisted: GET=%d calls=%d native=%d owner=%d", got.Code, f.generated.Load(), f.states.stateCalls.Load(), f.states.ownershipCalls.Load())
				}
				f.mu.Lock()
				parents := append([]string(nil), f.parents...)
				f.mu.Unlock()
				if len(parents) != 2 || parents[0] != "" || parents[1] != "parent_retention_1" {
					t.Fatalf("actual WS continuation=%v", parents)
				}
			})
		}
	}
}

func TestHTTPResponseStorageRejectsInvalidType(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []account.Provider{account.ProviderWeb, account.ProviderBuild, account.ProviderConsole} {
			t.Run(dialect+"/"+string(kind), func(t *testing.T) {
				f := newResponseRetentionFixture(t, dialect, kind)
				for _, value := range []any{"false", 0, []any{}, map[string]any{}} {
					output := f.create(t, false, map[string]any{"store": value}, "invalid")
					if output.Code != 400 || f.generated.Load() != 0 || f.states.stateCalls.Load() != 0 || f.states.ownershipCalls.Load() != 0 {
						t.Errorf("invalid store=%v status=%d calls=%d body=%s", value, output.Code, f.generated.Load(), output.Body.String())
					}
				}
			})
		}
	}
}
