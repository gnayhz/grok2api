package mediajob

import "github.com/chenyme/grok2api/backend/internal/domain/media"

// QuotaFinalizationModes separates the immediate local consumption fence from
// the authoritative provider refresh. A refresh group may update several
// upstream windows atomically, while the local fence must charge the exact
// window selected for this account.
func QuotaFinalizationModes(effectiveMode, refreshGroup string) (refreshMode, decrementMode, availabilityMode string) {
	if effectiveMode == "weekly" {
		return effectiveMode, effectiveMode, refreshGroup
	}
	refreshMode = effectiveMode
	if refreshGroup != "" {
		refreshMode = refreshGroup
	}
	return refreshMode, effectiveMode, ""
}

func VideoGenerated(job media.Job) bool {
	return job.Execution.Phase == media.VideoExecutionGenerated || job.Execution.Phase == "" && job.Status == media.StatusCompleted
}
