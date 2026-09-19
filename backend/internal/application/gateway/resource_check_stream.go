package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

// PrepareResourceCheck freezes the requested Build model and current rule.
func (s *Service) PrepareResourceCheck(ctx context.Context, kind string, id uint64, publicModel string) (qualitymodel.ProbeExperiment, error) {
	if kind == "account" {
		view, err := s.accounts.Get(ctx, id)
		if err != nil {
			return qualitymodel.ProbeExperiment{}, err
		}
		if view.Credential.Provider != account.ProviderBuild {
			return qualitymodel.ProbeExperiment{}, errors.New("quality checks require a Build account")
		}
		return s.prepareCheckModel(ctx, id, publicModel)
	}
	return s.prepareCheckModel(ctx, 0, publicModel)
}

func (s *Service) prepareCheckModel(ctx context.Context, accountID uint64, publicModel string) (qualitymodel.ProbeExperiment, error) {
	spec := qualitymodel.ProbeExperiment{Version: qualitymodel.ResourceCheckVersion, Sample: "token-short"}
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		return spec, err
	}
	for _, route := range routes {
		if !route.Enabled || route.Provider != account.ProviderBuild || !modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel) {
			continue
		}
		hold, _ := s.requestGuardSnapshot()
		if err := hold.Unavailable(); err != nil {
			return spec, err
		}
		spec.Baseline = attemptmeta.Identity{AccountID: accountID, Provider: string(route.Provider), Model: route.UpstreamModel, Revision: hold.Revision, RuleVersion: hold.RuleVersion}
		return spec, nil
	}
	return spec, ErrModelNotFound
}

// Resource measurements consume the bounded completed stream independently
// of early admission. The shared event interpretation retains thinking, usage
// and completion as separate facts; failed responses cannot prove degradation.
func readResourceCheckStream(ctx context.Context, body io.ReadCloser, hold QualityRetryRuntime, resources *selector.AttemptResources) (qualitymodel.ResourceSample, error) {
	stop := context.AfterFunc(ctx, resources.Close)
	defer stop()
	state := qualityScanState{kernel: hold.Kernel(), protocol: qualityProtocolResponses, startedAt: time.Now()}
	firstVerdict := QualityWait
	plainOutput := false
	unexpectedOutput := false
	checkItemType := func(kind string) {
		if kind != "" && kind != "message" && kind != "reasoning" {
			unexpectedOutput = true
		}
	}
	stream := responseflow.FromReader(body)
	if stream == nil {
		return qualitymodel.ResourceSample{}, errors.New("canonical check stream unavailable")
	}
	err := stream.ConsumeLimit(qualityProbeCompletionBytes, func(event *responseflow.Event) error {
		if event.HasData && !bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
			observeQualityPayload(&state, event.Data)
			var fact struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
				Item  struct {
					Type    string `json:"type"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"item"`
				Response struct {
					Output []struct {
						Type    string `json:"type"`
						Content []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"output"`
				} `json:"response"`
			}
			if json.Unmarshal(event.Data, &fact) == nil {
				checkItemType(fact.Item.Type)
				if strings.Contains(fact.Type, "_call.") || strings.Contains(fact.Type, "_call_") {
					unexpectedOutput = true
				}
				if fact.Type == "response.output_text.delta" && strings.TrimSpace(fact.Delta) != "" {
					plainOutput = true
				}
				for _, v := range fact.Item.Content {
					if v.Type == "output_text" && strings.TrimSpace(v.Text) != "" {
						plainOutput = true
					}
				}
				for _, item := range fact.Response.Output {
					checkItemType(item.Type)
					for _, v := range item.Content {
						if v.Type == "output_text" && strings.TrimSpace(v.Text) != "" {
							plainOutput = true
						}
					}
				}
			}
			if firstVerdict == QualityWait {
				firstVerdict, _ = state.streamVerdict(true)
			}
		}
		return state.protocolErr
	})
	state.terminal = true
	verdict, verdictErr := state.streamVerdict(true)
	if err == nil {
		err = verdictErr
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && (!state.completed || state.failed) {
		err = errors.New("successful completion not observed")
	}
	fp := state.fingerprint(verdict, err)
	sample := qualitymodel.ResourceSample{Outcome: qualitymodel.MeasurementError, Rule: fp.Rule,
		PlainOutput: plainOutput, UnexpectedOutput: unexpectedOutput, Conflict: firstVerdict == QualityWithhold && fp.HasThinking,
		Thinking: fp.HasThinking, Completed: fp.Completed && !fp.Failed, UsageReported: state.usage.Reported,
		InputTokens: state.usage.InputTokens, ReasoningTokens: state.usage.ReasoningTokens}
	if state.usage.CachedInputTokensReported {
		cached := state.usage.CachedInputTokens
		sample.CachedTokens = &cached
	}
	if err == nil {
		switch {
		case firstVerdict == QualityWithhold && sample.Thinking:
			// Late thinking cannot erase the earlier production rejection.
			// Retain both facts as conflicting evidence for manual review.
			sample.Failure = qualitymodel.ProbeFailureAdmission
		case verdict == QualityDeliver && sample.Thinking:
			sample.Outcome = qualitymodel.MeasurementClean
		case verdict == QualityWithhold:
			sample.Outcome = qualitymodel.MeasurementDegraded
		default:
			sample.Failure = qualitymodel.ProbeFailureAdmission
		}
	}
	return sample, err
}
