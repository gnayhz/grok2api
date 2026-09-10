package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

const completionJSON = `{"id":"resp_completion","model":"grok-4.6","status":"completed","output":[{"type":"reasoning","summary":[{"text":"plan"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":20,"output_tokens":5,"total_tokens":25,"output_tokens_details":{"reasoning_tokens":2}}}`

type completionOwnershipStore struct {
	responseHistory
	save func(context.Context, inferencedomain.ResponseOwnership) error
}

func (r completionOwnershipStore) Record(ctx context.Context, value inferencedomain.ResponseOwnership, _ time.Time) error {
	return r.save(ctx, value)
}

func completionService(t *testing.T, customize func(*provider.Response)) (*Service, *relational.AuditRepository) {
	t.Helper()
	base := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	s, _, records := newGuardLoopServiceWithDB(t, base, "completion")
	s.providers = provider.NewRegistry(resourceTestAdapter{base, func(ctx context.Context, _ provider.ResponseResourceRequest) (*provider.Response, error) {
		physicalCtx := attemptmeta.Begin(ctx, attemptmeta.Path{})
		if err := infraegress.BeginDirectPhysicalCall(physicalCtx); err != nil {
			return nil, err
		}
		raw := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(completionJSON))}
		infraegress.RecordDirectPhysicalCall(physicalCtx, raw, nil)
		response := &provider.Response{Attempt: attemptmeta.FromContext(physicalCtx), StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: raw.Body}
		if customize != nil {
			customize(response)
		}
		return response, nil
	}})
	return s, records
}

func readCompletion(t *testing.T, result *Result) {
	t.Helper()
	if _, err := io.Copy(io.Discard, result.Body); err != nil {
		t.Fatal(err)
	}
}

func oneCompletionAudit(t *testing.T, records *relational.AuditRepository) audit.Record {
	t.Helper()
	values, count, err := records.List(context.Background(), 0, 10)
	if err != nil || count != 1 {
		t.Fatalf("audit count=%d err=%v records=%+v", count, err, values)
	}
	return values[0]
}

func TestCompletionSeparatesRequiredCommitFailures(t *testing.T) {
	for _, stage := range []string{"none", "history", "state", "ownership", "ownership_without_history"} {
		t.Run(stage, func(t *testing.T) {
			var historyWrites, stateWrites, ownershipWrites atomic.Int32
			s, records := completionService(t, func(response *provider.Response) {
				if stage != "ownership_without_history" {
					response.CommitOutput = func() error {
						historyWrites.Add(1)
						if stage == "history" {
							return errors.New("history disk failure")
						}
						return nil
					}
				}
				if stage == "state" {
					response.CommitResponseState = func(context.Context) error { stateWrites.Add(1); return errors.New("native state disk failure") }
				}
			})
			s.responses = completionOwnershipStore{responseHistory: s.responses, save: func(context.Context, inferencedomain.ResponseOwnership) error {
				ownershipWrites.Add(1)
				if strings.HasPrefix(stage, "ownership") {
					return errors.New("ownership disk failure")
				}
				return nil
			}}
			input := guardLoopInput("completion-"+stage, false)
			input.Body = []byte(`{"model":"grok-4.6","input":"hello"}`)
			result, err := s.CreateResponse(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			readCompletion(t, result)
			usage := Usage{Reported: true, InputTokens: 20, OutputTokens: 5, TotalTokens: 25}
			err = result.CommitCompletion(Completion{Usage: usage, ResponseID: "resp_completion", NativeResponseID: "resp_completion"})
			if (err == nil) != (stage == "none") {
				t.Fatalf("stage=%s err=%v", stage, err)
			}
			if again := result.CommitCompletion(Completion{Usage: usage, ResponseID: "resp_completion", NativeResponseID: "resp_completion"}); again != err {
				t.Fatalf("unstable commit receipt: %v / %v", err, again)
			}
			// A caller cannot erase a failed necessary commit by passing success.
			result.Finalize(Usage{}, "", "")
			_ = result.Body.Close()
			record := oneCompletionAudit(t, records)
			wantHistory, wantState, wantOwnership, wantCode := "committed", "not_required", "committed", ""
			switch stage {
			case "history":
				wantHistory, wantOwnership, wantCode = "failed", "not_committed", "history_commit_failed"
			case "state":
				wantState, wantOwnership, wantCode = "failed", "not_committed", "provider_state_commit_failed"
			case "ownership":
				wantOwnership, wantCode = "failed", "response_ownership_commit_failed"
			case "ownership_without_history":
				wantHistory, wantOwnership, wantCode = "not_required", "failed", "response_ownership_commit_failed"
			}
			if record.HistoryCommit != wantHistory || record.ProviderStateCommit != wantState || record.OwnershipCommit != wantOwnership || record.ErrorCode != wantCode {
				t.Fatalf("independent receipts = %+v", record)
			}
			if record.GenerationOutcome != "completed" || record.InputTokens != 20 || record.OutputTokens != 5 || record.LedgerOutcome != "committed" || record.ResponseID != "resp_completion" {
				t.Fatalf("lost observed generation or usage: %+v", record)
			}
			if historyWrites.Load() > 1 || stateWrites.Load() > 1 || ownershipWrites.Load() > 1 {
				t.Fatal("finalization duplicated required writes")
			}
			if stage == "ownership_without_history" && ownershipWrites.Load() != 1 {
				t.Fatal("missing ownership barrier without history")
			}
		})
	}
}

func TestCancellationCannotFinalizeClaimedDeliveryWithEmptyUsage(t *testing.T) {
	s, records := completionService(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := s.CreateChatCompletion(ctx, guardLoopInput("cancel-observed", false))
	if err != nil {
		t.Fatal(err)
	}
	if err := result.BeginDelivery(); err != nil {
		t.Fatal(err)
	}
	readCompletion(t, result)
	cancel()
	// Cancellation must release the physical/account resources promptly while
	// transport finalization remains outstanding. No sleep guesses the race.
	deadline := time.Now().Add(time.Second)
	for {
		count, err := s.selector.concurrency.Current(context.Background(), accountConcurrencyKey(1))
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation retained account lease")
		}
		time.Sleep(time.Millisecond)
	}
	if _, count, err := records.List(context.Background(), 0, 1); err != nil || count != 0 {
		t.Fatalf("cancel stole completion: count=%d err=%v", count, err)
	}
	result.RecordDelivery(DeliveryStats{Bytes: 91, Events: 2})
	result.Finalize(Usage{Reported: true, InputTokens: 20, OutputTokens: 5, TotalTokens: 25}, "resp_cancel", "client_disconnected")
	_ = result.Body.Close()
	record := oneCompletionAudit(t, records)
	if record.AdmissionOutcome != "admitted" || record.DeliveryOutcome != "canceled" || record.StatusCode != 499 || record.InputTokens != 20 || record.DeliveredBytes != 91 || record.DeliveredEvents != 2 {
		t.Fatalf("cancel lost actual delivery metadata: %+v", record)
	}
}

func TestCancellationAfterCommitKeepsHistoryAndUsage(t *testing.T) {
	var writes atomic.Int32
	s, records := completionService(t, func(response *provider.Response) { response.CommitOutput = func() error { writes.Add(1); return nil } })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := s.CreateChatCompletion(ctx, guardLoopInput("cancel-after-commit", false))
	if err != nil {
		t.Fatal(err)
	}
	readCompletion(t, result)
	if err := result.CommitCompletion(Completion{Usage: Usage{Reported: true, InputTokens: 20, OutputTokens: 5}, ResponseID: "resp_committed", NativeResponseID: "resp_committed"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	result.RecordDelivery(DeliveryStats{Bytes: 42})
	result.Finalize(Usage{}, "", "client_disconnected")
	_ = result.Body.Close()
	record := oneCompletionAudit(t, records)
	if record.HistoryCommit != "committed" || record.GenerationOutcome != "completed" || record.InputTokens != 20 || record.ResponseID != "resp_committed" || writes.Load() != 1 {
		t.Fatalf("delivery failure rolled back a different stage: %+v writes=%d", record, writes.Load())
	}
}

type failCompletionReceipts struct{ fail atomic.Bool }

func (r *failCompletionReceipts) RecordPhysicalEvents(context.Context, []attemptmeta.PhysicalFact) error {
	if r.fail.Load() {
		return errors.New("physical receipt store unavailable")
	}
	return nil
}
func (r *failCompletionReceipts) RecordQualityEvent(_ context.Context, observation QualityObservation, _ time.Duration) error {
	if r.fail.Load() && observation.Outcome == QualityObservedCompleted {
		return errors.New("quality receipt store unavailable")
	}
	return nil
}

func TestReceiptFailureDoesNotRewriteGenerationOrDelivery(t *testing.T) {
	var accepted atomic.Int32
	s, records := completionService(t, func(response *provider.Response) { response.AcceptOutput = func() { accepted.Add(1) } })
	recorder := new(failCompletionReceipts)
	s.SetQualityEventRecorder(recorder)
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("receipt-independent", false))
	if err != nil {
		t.Fatal(err)
	}
	readCompletion(t, result)
	if err := result.CommitCompletion(Completion{Usage: Usage{Reported: true, InputTokens: 20, OutputTokens: 5}, ResponseID: "resp_receipt", NativeResponseID: "resp_receipt"}); err != nil {
		t.Fatal(err)
	}
	recorder.fail.Store(true)
	result.RecordDelivery(DeliveryStats{Bytes: 100, Events: 1})
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	record := oneCompletionAudit(t, records)
	if record.ErrorCode != "" || record.DeliveryOutcome != "completed" || record.GenerationOutcome != "completed" || record.PhysicalReceipt != "failed" || record.QualityReceipt != "failed" || accepted.Load() != 1 {
		t.Fatalf("receipt failure changed delivery: %+v accepted=%d", record, accepted.Load())
	}
}

func TestConcurrentCompletionAndCloseKeepAcknowledgedCommit(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s, records := completionService(t, func(response *provider.Response) {
		response.CommitOutput = func() error { close(started); <-release; return nil }
	})
	result, err := s.CreateChatCompletion(context.Background(), guardLoopInput("commit-close-race", false))
	if err != nil {
		t.Fatal(err)
	}
	readCompletion(t, result)
	var tasks sync.WaitGroup
	tasks.Add(2)
	go func() {
		defer tasks.Done()
		if err := result.CommitCompletion(Completion{Usage: Usage{Reported: true, OutputTokens: 5}, ResponseID: "resp_race", NativeResponseID: "resp_race"}); err != nil {
			t.Error(err)
		}
	}()
	<-started
	go func() { defer tasks.Done(); _ = result.Body.Close() }()
	close(release)
	tasks.Wait()
	record := oneCompletionAudit(t, records)
	if record.HistoryCommit != "committed" || record.OutputTokens != 5 || record.ResponseID != "resp_race" || record.ErrorCode != "stream_closed" {
		t.Fatalf("concurrent close erased commit receipt: %+v", record)
	}
}

func TestCompletionRejectsSyntheticOrMissingResourceIdentity(t *testing.T) {
	for _, native := range []string{"", "different-native"} {
		t.Run(native, func(t *testing.T) {
			var saved atomic.Int32
			s, records := completionService(t, nil)
			s.responses = completionOwnershipStore{responseHistory: s.responses, save: func(context.Context, inferencedomain.ResponseOwnership) error { saved.Add(1); return nil }}
			input := guardLoopInput("native-required", false)
			input.Body = []byte(`{"model":"grok-4.6","input":"hello"}`)
			result, err := s.CreateResponse(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			readCompletion(t, result)
			err = result.CommitCompletion(Completion{Usage: Usage{Reported: true, OutputTokens: 5}, ResponseID: "resp_abort", NativeResponseID: native})
			if !errors.Is(err, inferencedomain.ErrResponseOwnershipCommit) || saved.Load() != 0 {
				t.Fatalf("synthetic resource was persisted: %v calls=%d", err, saved.Load())
			}
			result.Finalize(Usage{}, "", "")
			_ = result.Body.Close()
			record := oneCompletionAudit(t, records)
			if record.OwnershipCommit != "failed" || record.ErrorCode != "response_ownership_commit_failed" {
				t.Fatalf("native identity failure mislabeled: %+v", record)
			}
		})
	}
}

func TestUnclaimedCancellationFinalizesAndClosesBody(t *testing.T) {
	s, records := completionService(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	result, err := s.CreateChatCompletion(ctx, guardLoopInput("unclaimed-cancel", false))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		_, count, err := records.List(context.Background(), 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("abandoned result leaked finalization")
		}
		time.Sleep(time.Millisecond)
	}
	if err := result.BeginDelivery(); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ownership transferred again: %v", err)
	}
	_ = result.Body.Close()
	record := oneCompletionAudit(t, records)
	if record.ErrorCode != "request_canceled" || record.AdmissionOutcome != "not_admitted" || record.DeliveryOutcome != "canceled" {
		t.Fatalf("abandonment facts=%+v", record)
	}
}
