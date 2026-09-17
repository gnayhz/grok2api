package evidence

import "github.com/chenyme/grok2api/backend/internal/quality/model"

func (s *Store) CrossValidate(snapshot model.Snapshot) model.Estimate {
	return model.CrossValidate(snapshot, s.config().MinWitnessObs)
}
