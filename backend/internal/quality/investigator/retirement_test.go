package investigator

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"testing"
)

func TestRetiredTaskCannotCallUpstream(t *testing.T) {
	e := NewProbeExecutor(nil)
	// No measurement/state dependencies: rejecting an old direction must happen
	// before any resource access, even when its saved result claimed degradation.
	for _, direction := range []model.ProbeDirection{model.ProbeAccountDifferential, model.ProbeExitJury, "unknown"} {
		got, err := e.Execute(context.Background(), model.ProbeTask{Direction: direction})
		if !errors.Is(err, ErrInadmissible) || got.Outcome != model.ProbeResultError || got.Detail != "unsupported_probe_direction" {
			t.Fatal(got, err)
		}
	}
}
