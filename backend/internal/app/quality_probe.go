package app

import "context"

// runQualityProbeWorker gives the investigator the application lifetime.
func (a *Application) runQualityProbeWorker(ctx context.Context) error {
	if a.qualityInvestigator == nil || a.qualityProbeExec == nil {
		<-ctx.Done()
		return nil
	}
	return a.qualityInvestigator.Run(ctx, a.qualityProbeExec, a.logger)
}
