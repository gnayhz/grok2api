// Package events owns durable request receipts and their incident-consumption
// policy. Storage transactions, evidence windows and case decisions retain their
// separate owners; the composition root only adapts and wires these ports.
package events

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/google/uuid"
)

type Journal interface {
	RecordMany(context.Context, []model.Event) error
	CheckCapacity(context.Context) error
	Stats(context.Context) (model.BacklogStats, error)
	Release(context.Context, string, time.Time) error
	ProcessOne(context.Context, string, func(context.Context, model.Event) error) (bool, error)
}
type Evidence interface {
	Record(context.Context, model.Observation) error
}
type Incidents interface {
	ReportDegradedObservation(context.Context, model.Observation) error
}

type Service struct {
	journal   Journal
	evidence  Evidence
	incidents Incidents
}

func New(store Journal, evidence Evidence, incidents Incidents) *Service {
	return &Service{journal: store, evidence: evidence, incidents: incidents}
}
func (s *Service) Backlog(ctx context.Context) (model.BacklogStats, error) {
	return s.journal.Stats(ctx)
}
func (s *Service) CheckQualityEventCapacity(ctx context.Context) error {
	return s.journal.CheckCapacity(ctx)
}

type Receipt struct {
	Attempt         attemptmeta.Identity
	Outcome         Outcome
	Rule, ErrorCode string
	At              time.Time
}
type Outcome string

// 持久化字符串值取自 quality/model 的三方共同合同常量
// (events 用例 / journal 校验 / 存量行解码)。
const (
	// Admitted 的持久值 "delivered" 表示准入通过,不是完成声明。
	Admitted    Outcome = model.EventOutcomeAdmitted
	Degraded    Outcome = model.EventOutcomeDegraded
	Rejected    Outcome = model.EventOutcomeRejected
	Completed   Outcome = model.EventOutcomeCompleted
	Interrupted Outcome = model.EventOutcomeInterrupted
	Canceled    Outcome = model.EventOutcomeCanceled
)

func (s *Service) RecordPhysicalEvents(ctx context.Context, facts []attemptmeta.PhysicalFact) error {
	events := make([]model.Event, 0, len(facts))
	for _, fact := range facts {
		event := model.Event{Attempt: fact.Attempt, Stage: model.EventStageExchange, Outcome: model.EventOutcomeObserved, At: fact.At, Physical: &fact}
		events = append(events, event)
	}
	return s.journal.RecordMany(ctx, events)
}

func (s *Service) RecordQualityEvent(ctx context.Context, obs Receipt, ttl time.Duration) error {
	stage := model.EventStageCompletion
	if obs.Outcome == Admitted || obs.Outcome == Degraded || obs.Outcome == Rejected {
		stage = model.EventStageAdmission
	}
	e := model.Event{Attempt: obs.Attempt, Stage: stage, Outcome: string(obs.Outcome), Rule: obs.Rule, ErrorCode: obs.ErrorCode, At: obs.At}
	if obs.Outcome == Degraded {
		e.HoldUntil = obs.At.Add(ttl)
	}
	if obs.Outcome == Degraded || obs.Outcome == Rejected {
		completion := model.Event{Attempt: obs.Attempt, Stage: model.EventStageCompletion, Outcome: string(Interrupted), At: obs.At, ErrorCode: obs.ErrorCode}
		if completion.ErrorCode == "" {
			completion.ErrorCode = "quality_degraded"
		}
		if completion.ErrorCode == "request_canceled" {
			completion.Outcome = string(Canceled)
		}
		return s.journal.RecordMany(ctx, []model.Event{e, completion})
	}
	return s.journal.RecordMany(ctx, []model.Event{e})
}

func (s *Service) handle(ctx context.Context, e model.Event) error {
	// Admission and completion remain in the journal. Merely observing thinking
	// cannot provide a healthy comparison sample, even if delivery later succeeds.
	// Only explicit rule violations enter the incident window from live traffic;
	// positive comparison evidence must come from a completed controlled probe.
	if e.Stage != "admission" || e.Attempt.Provider != "grok_build" {
		return nil
	}
	if e.Outcome != string(Degraded) {
		return nil
	}
	exit := model.EpochKey{}
	if !e.Attempt.Path.Rotating && e.Attempt.Path.Status == attemptmeta.PathRegistered {
		exit = model.EpochKey{NodeID: e.Attempt.Path.NodeID, Epoch: e.Attempt.Path.Epoch}
	}
	observation := model.Observation{EventID: e.ID(), Attempt: e.Attempt, At: e.At, AccountID: e.Attempt.AccountID, Exit: exit, Source: model.SourceTraffic, Outcome: model.OutcomeDegraded, Rule: e.Rule}
	if err := s.evidence.Record(ctx, observation); err != nil {
		return err
	}
	if err := s.incidents.ReportDegradedObservation(ctx, observation); err != nil {
		return err
	}
	return s.journal.Release(ctx, e.ID(), time.Now().UTC())
}

func (s *Service) Run(ctx context.Context) error {
	for {
		worked, err := s.journal.ProcessOne(ctx, uuid.NewString(), s.handle)
		if err != nil {
			return err
		} // supervisor backs off; the outbox retains the fact.
		if worked {
			continue
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
