package management

import (
	"context"
	"fmt"
	"time"

	qualitycourt "github.com/chenyme/grok2api/backend/internal/quality/court"
	qualityinvestigator "github.com/chenyme/grok2api/backend/internal/quality/investigator"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

// Runtime applies evidence first because expanding its window can fail. Case
// policy already captured when opening a case remains unchanged.
type Runtime struct {
	Court        interface{ SetConfig(qualitycourt.Config) }
	Investigator interface {
		SetConfig(qualityinvestigator.Config)
	}
	Evidence interface {
		SetConfigContext(context.Context, qualitymodel.EvidenceConfig) error
	}
}

func (s Runtime) Apply(ctx context.Context, t Config) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, d, err := normalize(t)
	if err != nil {
		return err
	}
	if s.Evidence != nil {
		if err := s.Evidence.SetConfigContext(ctx, qualitymodel.EvidenceConfig{
			Window: d.evidenceWindow, Retention: d.retention, MinWitnessObs: 1,
		}); err != nil {
			return fmt.Errorf("apply evidence settings: %w", err)
		}
	}
	if s.Court != nil {
		s.Court.SetConfig(qualitycourt.Config{
			AccountNeedExits:     t.AccountNeedExits,
			AccountSpanNodes:     t.AccountSpanNodes,
			ExitNeedN:            t.ExitNeedN,
			ExitNeedK:            t.ExitNeedK,
			JurorCount:           t.JurorsPerExit,
			InvestigationTimeout: d.investigationTimeout,
		})
	}
	if s.Investigator != nil {
		s.Investigator.SetConfig(qualityinvestigator.Config{
			DifferentialExits: t.DifferentialExits,
			JurorsPerExit:     t.JurorsPerExit,
			ProbeBudget:       t.ProbeBudget,
		})
	}
	return nil
}
