package investigator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type measurementCall struct {
	account, baseline, node uint64
	identity                model.ProbeIdentity
}
type recordedMeasurements struct {
	t      *testing.T
	task   model.ProbeTask
	calls  []measurementCall
	change func(int, *model.ProbeMeasurement)
}

func (r *recordedMeasurements) ProbeExitJury(ctx context.Context, account, node uint64) model.ProbeMeasurement {
	return r.measure(ctx, account, 0, node)
}
func (r *recordedMeasurements) ProbeAccountDifferentialOnPath(ctx context.Context, account, baseline, node uint64) model.ProbeMeasurement {
	return r.measure(ctx, account, baseline, node)
}
func (r *recordedMeasurements) measure(ctx context.Context, account, baseline, node uint64) model.ProbeMeasurement {
	r.t.Helper()
	spec, ok := model.ProbeExperimentFromContext(ctx)
	if !ok || spec != r.task.Experiment {
		r.t.Fatal("experiment did not reach measurement")
	}
	identity, ok := model.ProbeIdentityFromContext(ctx)
	if !ok {
		r.t.Fatal("measurement identity missing")
	}
	r.calls = append(r.calls, measurementCall{account, baseline, node, identity})
	actual := spec.Baseline
	actual.ID, actual.AccountID = fmt.Sprintf("physical/%d", len(r.calls)), account
	actual.Path = attemptmeta.Path{NodeID: node, Epoch: 1, Status: attemptmeta.PathRegistered}
	actual.Profile = spec.Profile()
	sample := model.ProbeMeasurement{Attempt: actual, Outcome: model.MeasurementDegraded, VerifiedIPChange: baseline != 0, PathKey: fmt.Sprintf("path-%d", node)}
	if len(r.calls) > 1 {
		sample.Outcome = model.MeasurementClean
	}
	if r.change != nil {
		r.change(len(r.calls), &sample)
	}
	return sample
}

func executorTask(direction model.ProbeDirection) model.ProbeTask {
	base := attemptmeta.Identity{ID: "opening", AccountID: 7, Provider: "grok_build", Model: "grok-4.6", Revision: 3, RuleVersion: "rules-v1", Path: attemptmeta.Path{NodeID: 8, Epoch: 1, Status: attemptmeta.PathRegistered}, Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "xhigh", Tools: true}}
	task := model.ProbeTask{ID: 1, Direction: direction, DefendantAccountID: 7, JurorAccountID: 9, DefendantNodeID: 10, DefendantEpoch: 1, BaselineNodeID: 8, BaselineEpoch: 1, ControlAccountID: 9, ControlNodeID: 10, ControlEpoch: 1, Experiment: model.NewProbeExperiment(model.Observation{EventID: "opening/admission", Attempt: base})}
	if direction == model.ProbeExitJury {
		task.DefendantNodeID, task.ControlNodeID = 8, 10
	}
	return task
}

func executorRegistries(t *testing.T, driver string) (*registry.Registry, *registry.Registry) {
	t.Helper()
	ctx := context.Background()
	opts := registry.Options{Driver: driver, SQLitePath: filepath.Join(t.TempDir(), "quality.db")}
	if driver == "postgres" {
		dsn := os.Getenv("GROK_EVOLUTION_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("requires isolated GROK_EVOLUTION_POSTGRES_DSN")
		}
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("probe_executor_%d", time.Now().UnixNano())
		if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			sqlDB.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Exec("DROP SCHEMA " + schema + " CASCADE"); sqlDB.Close() })
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		opts.PostgresDSN = dsn + separator + "search_path=" + schema
	}
	first, err := registry.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	for _, node := range []uint64{8, 10} {
		if _, _, err := first.AdvanceEpoch(ctx, node, model.ExitIdentityFromAggregate(fmt.Sprintf("198.51.100.%d", node))); err != nil {
			t.Fatal(err)
		}
	}
	second, err := registry.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	return first, second
}

func TestProbeExecutorReadsPeerEpochBeforeAndAfterMeasurements(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			for _, test := range []struct {
				name          string
				direction     model.ProbeDirection
				node          uint64
				at            int
				calls         int
				failure       bool
				controlDetail string
			}{
				{"comparison_before", model.ProbeAccountDifferential, 10, 0, 0, true, ""},
				{"baseline_before", model.ProbeAccountDifferential, 8, 0, 0, true, ""},
				{"comparison_during_primary", model.ProbeAccountDifferential, 10, 1, 1, true, ""},
				{"baseline_during_primary", model.ProbeAccountDifferential, 8, 1, 1, true, ""},
				{"comparison_during_control", model.ProbeAccountDifferential, 10, 2, 2, true, ""},
				{"baseline_during_control", model.ProbeAccountDifferential, 8, 2, 2, true, ""},
				{"jury_control_before", model.ProbeExitJury, 10, 0, 1, false, "control_epoch_stale"},
				{"jury_control_during_primary", model.ProbeExitJury, 10, 1, 1, false, "control_epoch_stale"},
				{"jury_control_during_control", model.ProbeExitJury, 10, 2, 2, false, "control_epoch_stale"},
				{"jury_defendant_during_control", model.ProbeExitJury, 8, 2, 2, true, ""},
			} {
				t.Run(test.name, func(t *testing.T) {
					a, b := executorRegistries(t, driver)
					task := executorTask(test.direction)
					advance := func() {
						if _, _, err := a.AdvanceEpoch(context.Background(), test.node, model.ExitIdentityFromAggregate("198.51.100.111")); err != nil {
							t.Fatal(err)
						}
					}
					calls := &recordedMeasurements{t: t, task: task, change: func(n int, _ *model.ProbeMeasurement) {
						if n == test.at {
							advance()
						}
					}}
					if test.at == 0 {
						advance()
					}
					result, err := NewProbeExecutor(b, calls, slog.New(slog.NewTextHandler(io.Discard, nil))).Execute(context.Background(), task)
					if err != nil || len(calls.calls) != test.calls || (result.Outcome == model.ProbeResultError) != test.failure || result.ControlVerified || result.ControlDetail != test.controlDetail {
						t.Fatalf("calls=%+v result=%+v err=%v", calls.calls, result, err)
					}
					if test.calls > 0 && result.Attempt.ID != "physical/1" {
						t.Fatalf("lost primary identity: %+v", result)
					}
					if test.calls > 1 && result.ControlAttempt.ID != "physical/2" {
						t.Fatalf("lost control identity: %+v", result)
					}
					if b.CurrentEpoch(test.node) != 1 {
						t.Fatal("test did not keep peer cache stale")
					}
				})
			}
		})
	}
}

type stateReadFailure struct {
	ProbeState
	reads, failAt int
	err           error
}

func (s *stateReadFailure) CurrentEpochAt(ctx context.Context, node uint64) (uint64, bool, error) {
	s.reads++
	if s.reads == s.failAt {
		return 0, false, s.err
	}
	return s.ProbeState.CurrentEpochAt(ctx, node)
}

func TestProbeExecutorReadFailureRetainsMeasuredFacts(t *testing.T) {
	for _, test := range []struct {
		name          string
		failAt, calls int
	}{{"before", 1, 0}, {"after_primary", 3, 1}, {"before_control", 5, 1}, {"after_control", 6, 2}} {
		t.Run(test.name, func(t *testing.T) {
			_, reg := executorRegistries(t, "sqlite")
			task := executorTask(model.ProbeAccountDifferential)
			calls := &recordedMeasurements{t: t, task: task}
			failure := errors.New("database read failed")
			state := &stateReadFailure{ProbeState: reg, failAt: test.failAt, err: failure}
			result, err := NewProbeExecutor(state, calls, nil).Execute(context.Background(), task)
			if !errors.Is(err, failure) || result.FailureKind != string(model.ProbeFailurePersistence) || len(calls.calls) != test.calls || result.Outcome != model.ProbeResultError || result.ControlVerified {
				t.Fatalf("result=%+v calls=%d err=%v", result, len(calls.calls), err)
			}
			if test.calls > 0 && result.Attempt.ID == "" {
				t.Fatal("lost primary")
			}
			if test.calls > 1 && result.ControlAttempt.ID == "" {
				t.Fatal("lost control")
			}
		})
	}
}

func TestProbeExecutorCancellationRetainsBothMeasurements(t *testing.T) {
	for _, at := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			_, reg := executorRegistries(t, "sqlite")
			task := executorTask(model.ProbeExitJury)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := &recordedMeasurements{t: t, task: task, change: func(n int, _ *model.ProbeMeasurement) {
				if n == at {
					cancel()
				}
			}}
			if at == 0 {
				cancel()
			}
			result, err := NewProbeExecutor(reg, calls, nil).Execute(ctx, task)
			if !errors.Is(err, context.Canceled) || len(calls.calls) != at || result.Outcome != model.ProbeResultError || result.FailureKind != string(model.ProbeFailureInterrupted) || result.ControlVerified {
				t.Fatalf("result=%+v calls=%d err=%v", result, len(calls.calls), err)
			}
			if at > 0 && result.Attempt.ID == "" {
				t.Fatal("lost primary")
			}
			if at > 1 && result.ControlAttempt.ID == "" {
				t.Fatal("lost control")
			}
		})
	}
}

func TestProbeExecutorQualifications(t *testing.T) {
	for _, direction := range []model.ProbeDirection{model.ProbeAccountDifferential, model.ProbeExitJury} {
		t.Run(string(direction), func(t *testing.T) {
			for _, test := range []struct {
				name    string
				mutate  func(*model.ProbeMeasurement)
				main    bool
				failure model.ProbeFailure
			}{
				{"matched", func(*model.ProbeMeasurement) {}, false, ""},
				{"clean_no_control", func(s *model.ProbeMeasurement) { s.Outcome = model.MeasurementClean }, true, ""},
				{"raw_error", func(s *model.ProbeMeasurement) {
					s.Outcome = model.MeasurementError
					s.Reason = "secret upstream error"
					s.Failure = model.ProbeFailureHTTPServer
				}, true, model.ProbeFailureHTTPServer},
				{"account", func(s *model.ProbeMeasurement) { s.Attempt.AccountID++ }, true, model.ProbeFailureIdentity},
				{"node", func(s *model.ProbeMeasurement) { s.Attempt.Path.NodeID++ }, true, model.ProbeFailureIdentity},
				{"epoch", func(s *model.ProbeMeasurement) { s.Attempt.Path.Epoch++ }, true, model.ProbeFailureIdentity},
				{"rotating", func(s *model.ProbeMeasurement) { s.Attempt.Path.Rotating = true }, true, model.ProbeFailureIdentity},
				{"unknown_path", func(s *model.ProbeMeasurement) { s.Attempt.Path.Status = attemptmeta.PathUnknown }, true, model.ProbeFailureIdentity},
				{"missing_id", func(s *model.ProbeMeasurement) { s.Attempt.ID = "" }, true, model.ProbeFailureIdentity},
				{"model", func(s *model.ProbeMeasurement) { s.Attempt.Model = "another" }, true, model.ProbeFailureExperiment},
				{"provider", func(s *model.ProbeMeasurement) { s.Attempt.Provider = "another" }, true, model.ProbeFailureExperiment},
				{"revision", func(s *model.ProbeMeasurement) { s.Attempt.Revision++ }, true, model.ProbeFailureExperiment},
				{"rules", func(s *model.ProbeMeasurement) { s.Attempt.RuleVersion = "another" }, true, model.ProbeFailureExperiment},
				{"profile", func(s *model.ProbeMeasurement) { s.Attempt.Profile.Tools = true }, true, model.ProbeFailureExperiment},
				{"sample", func(s *model.ProbeMeasurement) { s.Attempt.Profile.Sample = "another" }, true, model.ProbeFailureExperiment},
				{"control_account", func(s *model.ProbeMeasurement) { s.Attempt.AccountID++ }, false, ""},
				{"control_sample", func(s *model.ProbeMeasurement) { s.Attempt.Profile.Sample = "another" }, false, ""},
				{"control_version", func(s *model.ProbeMeasurement) { s.Attempt.Profile.Experiment = "another" }, false, ""},
				{"control_path", func(s *model.ProbeMeasurement) { s.PathKey = "" }, false, ""},
				{"control_epoch", func(s *model.ProbeMeasurement) { s.Attempt.Path.Epoch++ }, false, ""},
				{"control_policy", func(s *model.ProbeMeasurement) { s.Attempt.RuleVersion = "another" }, false, ""},
				{"control_same_attempt", func(s *model.ProbeMeasurement) { s.Attempt.ID = "physical/1" }, false, ""},
			} {
				t.Run(test.name, func(t *testing.T) {
					_, reg := executorRegistries(t, "sqlite")
					task := executorTask(direction)
					calls := &recordedMeasurements{t: t, task: task, change: func(n int, s *model.ProbeMeasurement) {
						if (test.main && n == 1) || (!test.main && n == 2) {
							test.mutate(s)
						}
					}}
					result, err := NewProbeExecutor(reg, calls, slog.New(slog.NewTextHandler(io.Discard, nil))).Execute(context.Background(), task)
					if err != nil || result.FailureKind != string(test.failure) {
						t.Fatalf("result=%+v err=%v", result, err)
					}
					if test.name == "clean_no_control" {
						if len(calls.calls) != 1 || result.Outcome != model.ProbeResultClean {
							t.Fatalf("result=%+v calls=%v", result, calls.calls)
						}
						return
					}
					wantCalls := 2
					if test.failure == model.ProbeFailureIdentity || test.failure == model.ProbeFailureExperiment {
						wantCalls = 1
					}
					if len(calls.calls) != wantCalls {
						t.Fatalf("calls=%v", calls.calls)
					}
					wantVerified := test.name == "matched" || test.name == "raw_error"
					if result.ControlVerified != wantVerified {
						t.Fatalf("result=%+v", result)
					}
					if strings.Contains(result.Detail, "secret") {
						t.Fatal("provider text persisted")
					}
					if wantCalls == 1 {
						if result.ControlAttempt.ID != "" || result.ControlOutcome != "" {
							t.Fatal("fabricated control without a measurement")
						}
						return
					}
					primary, control := calls.calls[0], calls.calls[1]
					if direction == model.ProbeAccountDifferential {
						if primary.account != 7 || primary.baseline != 8 || primary.node != 10 || control.account != 9 || control.baseline != 0 || control.node != 10 {
							t.Fatalf("calls=%v", calls.calls)
						}
					} else {
						if primary.account != 9 || primary.baseline != 0 || primary.node != 8 || control.account != 9 || control.baseline != 8 || control.node != 10 {
							t.Fatalf("calls=%v", calls.calls)
						}
					}
				})
			}
		})
	}
}

func TestProbeExecutorRejectsUnsupportedTaskWithoutMeasurement(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*model.ProbeTask)
	}{
		{"version", func(v *model.ProbeTask) { v.Experiment.Version = "future" }},
		{"sample", func(v *model.ProbeTask) { v.Experiment.Sample = "missing" }},
		{"legacy_profile", func(v *model.ProbeTask) {
			v.Experiment.Version = model.LegacyProbeExperimentVersion
			v.Experiment.Baseline.Profile.Tools = true
		}},
		{"direction", func(v *model.ProbeTask) { v.Direction = "unknown" }},
		{"baseline_missing", func(v *model.ProbeTask) { v.BaselineNodeID = 0 }},
		{"node_missing", func(v *model.ProbeTask) { v.DefendantNodeID = 0 }},
		{"epoch_unknown", func(v *model.ProbeTask) { v.DefendantNodeID = 300; v.DefendantEpoch = 0 }},
		{"account_missing", func(v *model.ProbeTask) { v.DefendantAccountID = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, reg := executorRegistries(t, "sqlite")
			task := executorTask(model.ProbeAccountDifferential)
			test.mutate(&task)
			calls := &recordedMeasurements{t: t, task: task}
			result, _ := NewProbeExecutor(reg, calls, nil).Execute(context.Background(), task)
			if result.Outcome != model.ProbeResultError || len(calls.calls) != 0 {
				t.Fatalf("result=%+v calls=%v", result, calls.calls)
			}
		})
	}
}
