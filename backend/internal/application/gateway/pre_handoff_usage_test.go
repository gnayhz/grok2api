package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestTextGenerationUsageSurvivesPreHandoffFailure(t *testing.T) {
	for _, stage := range []string{"conversion", "quality_receipt", "cancel_during_conversion", "unread_close"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, records := completionService(t, func(response *provider.Response) {
				if stage == "conversion" || stage == "cancel_during_conversion" {
					response.ConvertJSON = func([]byte) ([]byte, error) {
						if stage == "cancel_during_conversion" {
							cancel()
						}
						return nil, errors.New("injected conversion failure")
					}
				}
			})
			s.UpdateQualityRetry(QualityRetryRuntime{Enabled: stage == "quality_receipt"})
			if stage == "quality_receipt" {
				s.SetQualityEventRecorder(eventRecorderFunc(func(_ context.Context, _ QualityObservation, _ time.Duration) error {
					return errors.New("injected quality receipt failure")
				}))
			}
			result, err := s.CreateResponse(ctx, guardLoopInput("pre-handoff-"+stage, false))
			if result != nil {
				_ = result.Body.Close()
			}
			if stage != "unread_close" && err == nil {
				t.Fatal("expected pre-handoff failure")
			}
			if stage == "unread_close" && err != nil {
				t.Fatal(err)
			}
			record := oneCompletionAudit(t, records)
			record, detailErr := records.Get(context.Background(), record.ID)
			if detailErr != nil {
				t.Fatal(detailErr)
			}
			if len(record.GenerationUsages) != 1 || !record.GenerationUsages[0].Selected || record.GenerationUsages[0].InputTokens != 20 {
				t.Fatalf("missing attempt usage: %+v", record.GenerationUsages)
			}
			if record.InputTokens != 20 || record.OutputTokens != 5 || record.GenerationOutcome != "completed" {
				t.Fatalf("lost confirmed generation: input=%d output=%d generation=%s error=%s", record.InputTokens, record.OutputTokens, record.GenerationOutcome, record.ErrorCode)
			}
			if record.DeliveredBytes != 0 || record.DeliveryOutcome == "completed" {
				t.Fatalf("invented delivery: %+v", record)
			}
		})
	}
}
