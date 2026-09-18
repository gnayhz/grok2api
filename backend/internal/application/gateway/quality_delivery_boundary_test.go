package gateway

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestGuardConversionFailureNeverCommitsOrDeliversRawProtocol(t *testing.T) {
	for _, empty := range []bool{false, true} {
		adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
		service, credentials, limiter := newGuardLoopServiceWithLimiter(t, adapter, "convert-failure")
		accepted := false
		var events []QualityObservation
		service.SetQualityEventRecorder(eventRecorderFunc(func(ctx context.Context, obs QualityObservation, _ time.Duration) error {
			if obs.Outcome == QualityObservedInterrupted {
				if count, _ := limiter.Current(ctx, repository.AccountConcurrencyKey(credentials[0].ID)); count != 0 {
					t.Error("conversion failure retained account lease during receipt")
				}
			}
			events = append(events, obs)
			return nil
		}))
		adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{
			status:       http.StatusOK,
			body:         `{"id":"raw","output":[{"type":"reasoning","summary":[{"text":"plan"}]},{"type":"message","content":[{"type":"output_text","text":"RAW_PROTOCOL"}]}]}`,
			acceptOutput: func() { accepted = true },
			convertJSON: func([]byte) ([]byte, error) {
				if empty {
					return nil, nil
				}
				return nil, errors.New("converter failure")
			},
		}}
		result, err := service.CreateMessage(context.Background(), guardLoopInput("conversion-failed", false))
		if result != nil {
			finishTestResult(t, result, Usage{}, "", "response_conversion_failed")
			_ = result.Body.Close()
		}
		if err == nil || result != nil || accepted {
			t.Fatalf("empty=%v: result=%v err=%v accepted=%v; conversion must fail before commit", empty, result != nil, err, accepted)
		}
		if len(events) != 2 || events[0].Outcome != QualityObservedAdmitted || events[1].Outcome != QualityObservedInterrupted || events[1].ErrorCode != "response_conversion_failed" || events[0].Attempt != events[1].Attempt {
			t.Fatalf("conversion failure did not preserve separate admission/completion: %+v", events)
		}
	}
}

func TestGuardRescueRequiresSuccessfulDelivery(t *testing.T) {
	for _, outcome := range []string{"", "upstream_stream_interrupted", "client_disconnected"} {
		adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
		service, credentials := newGuardLoopService(t, adapter, "rescue-first", "rescue-second")
		adapter.responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: "data: {\"choices\":[{\"delta\":{\"content\":\"bare\"}}]}\n\n"}}
		adapter.responses[credentials[1].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"plan\"}}]}\n\ndata: [DONE]\n\n"}}
		before := findGuardSignalStat(t, processGuardStatsSnapshot(), GuardSignalWithhold)
		result, err := service.CreateChatCompletion(context.Background(), guardLoopInput("rescue-delivery", true))
		if err != nil {
			t.Fatal(err)
		}
		pending := findGuardSignalStat(t, processGuardStatsSnapshot(), GuardSignalWithhold)
		if pending.Rescued != before.Rescued || pending.Failed != before.Failed {
			t.Error("request was classified as completed before delivery finalized")
		}
		_, _ = io.Copy(io.Discard, result.Body)
		finishTestResult(t, result, Usage{}, "", outcome)
		result.Finalize(Usage{}, "", outcome)
		_ = result.Body.Close()
		after := findGuardSignalStat(t, processGuardStatsSnapshot(), GuardSignalWithhold)
		wantRescued, wantFailed := int64(0), int64(1)
		if outcome == "" {
			wantRescued, wantFailed = 1, 0
		}
		if after.Rescued-before.Rescued != wantRescued || after.Failed-before.Failed != wantFailed {
			t.Fatalf("outcome=%q rescued=%d failed=%d", outcome, after.Rescued-before.Rescued, after.Failed-before.Failed)
		}
	}
}

func TestDeferredConversionReadErrorIsNotEmptySuccess(t *testing.T) {
	response := &provider.Response{
		Body:        io.NopCloser(io.MultiReader(strings.NewReader("partial"), guardReadFailure{})),
		ConvertJSON: func([]byte) ([]byte, error) { return []byte(`{}`), nil },
	}
	if err := applyDeferredStreamConversion(response); err == nil {
		t.Fatal("body read failure must be propagated")
	}
}

func TestNonstreamFailedTerminalCannotCommitConversationOrCompleteSuccessfully(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "cancelled"} {
		for _, convert := range []bool{false, true} {
			adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
			service, credentials := newGuardLoopService(t, adapter, "failed-json-terminal")
			var events []QualityObservation
			service.SetQualityEventRecorder(eventRecorderFunc(func(_ context.Context, obs QualityObservation, _ time.Duration) error {
				events = append(events, obs)
				return nil
			}))
			accepted := false
			response := scriptedBuildResponse{status: http.StatusOK,
				body:         `{"status":"` + status + `","output":[{"type":"reasoning","summary":[{"text":"plan"}]}]}`,
				acceptOutput: func() { accepted = true }}
			if convert {
				response.convertJSON = func([]byte) ([]byte, error) { return []byte(`{"choices":[{"message":{"content":"partial"}}]}`), nil }
			}
			adapter.responses[credentials[0].ID] = []scriptedBuildResponse{response}
			result, err := service.CreateResponse(context.Background(), guardLoopInput("failed-json-terminal", false))
			if result != nil {
				finishTestResult(t, result, Usage{}, "", "")
				_ = result.Body.Close()
			}
			// A complete failure envelope is rejected before admission, even if
			// it carries thinking; no successful completion or cache write exists.
			if !errors.Is(err, errQualityUpstreamFailure) || result != nil || accepted || len(events) != 1 || events[0].Outcome != QualityObservedRejected || events[0].ErrorCode != "upstream_error" {
				t.Errorf("status=%s convert=%v result=%v err=%v accepted=%v events=%+v", status, convert, result != nil, err, accepted, events)
			}
		}
	}
}

func guardLoopInput(requestID string, streaming bool) Input {
	return Input{RequestID: requestID, ClientKey: clientkey.Key{ModelScope: clientkey.ModelScopeAll, ID: 1, Name: "k"}, PublicModel: "grok-4.6", Streaming: streaming, Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hello"}]}`)}
}

type guardReadFailure struct{}

func (guardReadFailure) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
