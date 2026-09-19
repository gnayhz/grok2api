package court

import (
	"context"

	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type simpleTaskDispatcher struct {
	store *registry.ProbeTaskStore
}

type recordingSimpleTaskDispatcher struct {
	store *registry.ProbeTaskStore
	specs []DispatchSpec
}

func (d *recordingSimpleTaskDispatcher) DispatchForCase(ctx context.Context, spec DispatchSpec) (int, error) {
	d.specs = append(d.specs, spec)
	return (simpleTaskDispatcher{store: d.store}).DispatchForCase(ctx, spec)
}
func (d simpleTaskDispatcher) DispatchForCase(ctx context.Context, spec DispatchSpec) (int, error) {
	_, err := d.store.CreateProbeTask(ctx, model.ProbeTask{CaseID: spec.CaseID, Direction: model.ProbeCaseProof, DefendantAccountID: spec.Defendant, DefendantNodeID: spec.BaselineExit.NodeID, DefendantEpoch: spec.BaselineExit.Epoch, BaselineNodeID: spec.BaselineExit.NodeID, BaselineEpoch: spec.BaselineExit.Epoch})
	return 1, err
}
func reportTestIncident(s *Service, ctx context.Context, account uint64, exit model.EpochKey) error {
	return s.ReportDegradedObservation(ctx, model.Observation{At: time.Now().UTC(), AccountID: account, Exit: exit, Attempt: experimentIdentity("fictional-trigger", account, exit.NodeID, exit.Epoch)})
}

func openSimpleTestCase(t *testing.T, service *Service, reg *registry.Registry) uint64 {
	t.Helper()
	if err := reportTestIncident(service, context.Background(), 7, model.EpochKey{NodeID: 3, Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	cases, err := reg.ListOpenCases(context.Background())
	if err != nil || len(cases) != 1 {
		t.Fatalf("simple case not opened: cases=%d err=%v", len(cases), err)
	}
	if reg.AccountState(7).State != model.AccountRemanded || reg.ExitStateOfCurrentEpoch(3).State != model.ExitRemanded {
		t.Fatalf("incident must freeze both parties: account=%+v exit=%+v", reg.AccountState(7), reg.ExitStateOfCurrentEpoch(3))
	}
	return cases[0].ID
}

// Translate the synthetic account/exit world into the same complete proof
// certificates used by the production executor. No vote counts are involved.
func settleSimpleTestTasks(t *testing.T, reg *registry.Registry, caseID uint64, result func(model.ProbeTaskView) model.ProbeTaskResult) {
	t.Helper()
	ctx := context.Background()
	store := registry.NewProbeTaskStore(reg)
	claimed, err := store.ClaimPendingProbeTasks(ctx, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) == 0 {
		t.Fatal("case must create proof task")
	}
	for _, task := range claimed {
		if task.CaseID != caseID {
			continue
		}
		account := result(model.ProbeTaskView{Direction: model.ProbeAccountDifferential})
		exit := result(model.ProbeTaskView{Direction: model.ProbeExitJury})
		now := time.Now().UTC()
		var observations []model.ResourceObservation
		if account.Outcome != model.ProbeResultError && exit.Outcome != model.ProbeResultError {
			a, n := task.DefendantAccountID, task.BaselineNodeID
			otherA, otherN := uint64(8), uint64(4)
			if otherA == a {
				otherA++
			}
			if otherN == n {
				otherN++
			}
			ac, ec := "A", "A"
			if account.Outcome == model.ProbeResultDegraded {
				ac = "B"
			}
			if exit.Outcome == model.ProbeResultDegraded {
				ec = "B"
			}
			observations = []model.ResourceObservation{caseProofObservation(task.Experiment, 1, otherA, otherN, "A"), caseProofObservation(task.Experiment, 2, a, otherN, ac), caseProofObservation(task.Experiment, 3, otherA, n, ec)}
		}
		r := caseProofReport(task.Experiment, now, observations)
		for i := 1; i <= len(observations); i++ {
			r.Calls, r.Revision = i, uint64(i)
			if err := store.SaveResourceCheckProgress(ctx, task.ID, r); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.CompleteProbeTask(ctx, task.ID, model.ProbeDone, model.ProbeTaskResult{ResourceCheck: &r}, now); err != nil {
			t.Fatal(err)
		}
	}
}

func settleSimpleCaseUntilClosed(t *testing.T, service *Service, reg *registry.Registry, caseID uint64, result func(model.ProbeTaskView) model.ProbeTaskResult) {
	t.Helper()
	store := registry.NewProbeTaskStore(reg)
	for attempt := 0; attempt < 8; attempt++ {
		tasks, err := store.ListProbeTasksForCase(context.Background(), caseID)
		if err != nil {
			t.Fatal(err)
		}
		pending := false
		for _, task := range tasks {
			if task.State == model.ProbePending {
				pending = true
			}
		}
		if pending {
			settleSimpleTestTasks(t, reg, caseID, result)
		}
		if _, err := service.Evaluate(context.Background(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		caseRecord, found, err := reg.GetCase(context.Background(), caseID)
		if err != nil {
			t.Fatal(err)
		}
		if found && caseRecord.Status.Closed() {
			return
		}
	}
	t.Fatalf("case %d did not close after bounded replacement rounds", caseID)
}

func TestSimpleCourtExitConvictionBansIncidentExit(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	caseID := openSimpleTestCase(t, service, bench.registry)
	settleSimpleTestTasks(t, bench.registry, caseID, func(task model.ProbeTaskView) model.ProbeTaskResult {
		if task.Direction == model.ProbeExitJury {
			return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded, Detail: "jury_degraded"}
		}
		return model.ProbeTaskResult{Outcome: model.ProbeResultClean, Detail: "comparison_clean"}
	})
	if _, err := service.Evaluate(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if state := bench.registry.ExitStateOfCurrentEpoch(3).State; state != model.ExitBanned {
		t.Fatalf("jury quorum must ban incident exit, state=%s", state)
	}
	if !bench.registry.AccountEligible(7) {
		t.Fatal("exit conviction must release the account")
	}
	records, err := bench.registry.ListRecentCases(context.Background(), 10)
	if err != nil || len(records) != 1 || records[0].Verdict != model.VerdictExitGuilty {
		t.Fatalf("exit conviction must close with explicit verdict: records=%+v err=%v", records, err)
	}
}

func TestSimpleCourtAccountConvictionFreezesIncidentAccount(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	caseID := openSimpleTestCase(t, service, bench.registry)
	settleSimpleTestTasks(t, bench.registry, caseID, func(task model.ProbeTaskView) model.ProbeTaskResult {
		if task.Direction == model.ProbeExitJury {
			return model.ProbeTaskResult{Outcome: model.ProbeResultClean, Detail: "jury_clean"}
		}
		return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded, VerifiedIPChange: true, Detail: "differential_degraded"}
	})
	if _, err := service.Evaluate(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if state := bench.registry.AccountState(7).State; state != model.AccountSentenced {
		t.Fatalf("differential quorum must sentence account, state=%s", state)
	}
	if bench.registry.ExitStateOfCurrentEpoch(3).State != model.ExitAvailable {
		t.Fatal("account conviction must release the incident exit")
	}
	records, err := bench.registry.ListRecentCases(context.Background(), 10)
	if err != nil || len(records) != 1 || records[0].Verdict != model.VerdictAccountGuilty {
		t.Fatalf("account conviction must close with explicit verdict: records=%+v err=%v", records, err)
	}
}

func TestSimpleCourtTransportErrorsReleaseImmediately(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	caseID := openSimpleTestCase(t, service, bench.registry)
	settleSimpleCaseUntilClosed(t, service, bench.registry, caseID, func(model.ProbeTaskView) model.ProbeTaskResult {
		return model.ProbeTaskResult{Outcome: model.ProbeResultError, Detail: "created_timeout"}
	})
	if !bench.registry.AccountEligible(7) || !bench.registry.ExitEligible(3) {
		t.Fatal("inconclusive investigation must release both parties after bounded replacements")
	}
	records, err := bench.registry.ListRecentCases(context.Background(), 10)
	if err != nil || len(records) != 1 || records[0].Verdict != model.VerdictInsufficient || records[0].Status != model.CaseDismissed {
		t.Fatalf("transport errors must produce an explicit insufficient closure after replacement exhaustion: records=%+v err=%v", records, err)
	}
}

func TestSimpleCourtDeadlineClosesStuckPendingCase(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	cfg.InvestigationTimeout = time.Nanosecond
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	caseID := openSimpleTestCase(t, service, bench.registry)
	// No worker claims the initial pending tasks. The deadline must still
	// cancel them and release both parties in this evaluation pass.
	if _, err := service.Evaluate(context.Background(), time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if bench.registry.AccountState(7).State != model.AccountActive || !bench.registry.ExitEligible(3) {
		t.Fatal("deadline closure must release remanded account and exit")
	}
	records, err := bench.registry.ListRecentCases(context.Background(), 10)
	if err != nil || len(records) != 1 || records[0].ID != caseID || records[0].Verdict != model.VerdictInsufficient {
		t.Fatalf("stuck pending case must close as insufficient: records=%+v err=%v", records, err)
	}
	tasks, err := store.ListProbeTasksForCase(context.Background(), caseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.State != model.ProbeCancelled {
			t.Fatalf("deadline must cancel unfinished probe %d, state=%s", task.ID, task.State)
		}
	}
}

func TestSimpleCourtCoalescesRepeatedIncident(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	first := model.EpochKey{NodeID: 3, Epoch: 0}
	if err := reportTestIncident(service, context.Background(), 7, first); err != nil {
		t.Fatal(err)
	}
	if err := reportTestIncident(service, context.Background(), 7, first); err != nil {
		t.Fatal(err)
	}
	cases, err := bench.registry.ListOpenCases(context.Background())
	if err != nil || len(cases) != 1 {
		t.Fatalf("same incident must use one case: cases=%d err=%v", len(cases), err)
	}
	parties, err := bench.registry.ListParties(context.Background(), cases[0].ID)
	if err != nil || len(parties) != 2 {
		t.Fatalf("same incident must not duplicate parties: parties=%d err=%v", len(parties), err)
	}
	tasks, err := store.ListProbeTasksForCase(context.Background(), cases[0].ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("same incident must not duplicate the probe round: tasks=%d err=%v", len(tasks), err)
	}
}

func TestSimpleCourtStartsSeparateCaseForSameAccountOnAnotherExit(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	if err := reportTestIncident(service, context.Background(), 7, model.EpochKey{NodeID: 3, Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	if err := reportTestIncident(service, context.Background(), 7, model.EpochKey{NodeID: 4, Epoch: 0}); err != nil {
		t.Fatal(err)
	}

	cases, err := bench.registry.ListOpenCases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("same account on different exits must create separate cases, got %d", len(cases))
	}
	for _, record := range cases {
		parties, listErr := bench.registry.ListParties(context.Background(), record.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		exitParties := 0
		for _, party := range parties {
			if party.Kind == model.PartyAccount && party.AccountID != 7 {
				t.Fatalf("unexpected defendant party: %+v", party)
			}
			if party.Kind == model.PartyExit {
				exitParties++
			}
		}
		if len(parties) != 2 || exitParties != 1 {
			t.Fatalf("each incident must have exactly one account and one exit party: case=%d parties=%+v", record.ID, parties)
		}
		tasks, taskErr := store.ListProbeTasksForCase(context.Background(), record.ID)
		if taskErr != nil {
			t.Fatal(taskErr)
		}
		if len(tasks) != 1 {
			t.Fatalf("each incident must receive one finite probe round: case=%d tasks=%d", record.ID, len(tasks))
		}
	}
	if bench.registry.AccountState(7).State != model.AccountRemanded {
		t.Fatalf("account must remain held while either case is open, state=%s", bench.registry.AccountState(7).State)
	}
	if bench.registry.ExitStateOfCurrentEpoch(3).State != model.ExitRemanded || bench.registry.ExitStateOfCurrentEpoch(4).State != model.ExitRemanded {
		t.Fatal("both incident exits must remain held by their own cases")
	}
}

func TestSimpleCourtClosureSuppressionIsScopedToIncidentExit(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	ctx := context.Background()

	if err := reportTestIncident(service, ctx, 7, model.EpochKey{NodeID: 3, Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	if err := reportTestIncident(service, ctx, 7, model.EpochKey{NodeID: 4, Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	cases, err := bench.registry.ListOpenCases(ctx)
	if err != nil || len(cases) != 2 {
		t.Fatalf("expected two independent incident cases, cases=%d err=%v", len(cases), err)
	}
	var first, second model.CaseRecord
	for _, record := range cases {
		parties, listErr := bench.registry.ListParties(ctx, record.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, party := range parties {
			if party.Kind != model.PartyExit {
				continue
			}
			switch party.NodeID {
			case 3:
				first = record
			case 4:
				second = record
			}
		}
	}
	if first.ID == 0 || second.ID == 0 {
		t.Fatalf("could not identify independent incident cases: first=%+v second=%+v", first, second)
	}

	settleSimpleTestTasks(t, bench.registry, first.ID, func(model.ProbeTaskView) model.ProbeTaskResult {
		return model.ProbeTaskResult{Outcome: model.ProbeResultError, Detail: "first_case_inconclusive"}
	})
	if _, err := service.Evaluate(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// This event happened after the first case closed, but before the second
	// case closes. A later closure for node 4 must not suppress it.
	eventAt := time.Now().UTC()
	if err := bench.evidence.Record(ctx, model.Observation{
		At: eventAt, AccountID: 7, Exit: model.EpochKey{NodeID: 3, Epoch: 0},
		Source: model.SourceTraffic, Outcome: model.OutcomeDegraded, Rule: "test",
	}); err != nil {
		t.Fatal(err)
	}
	secondParties, err := bench.registry.ListParties(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := settleTestInsufficient(service, ctx, second, secondParties, "test_second_case_close", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Evaluate(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	openCases, err := bench.registry.ListOpenCases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(openCases) != 1 {
		t.Fatalf("new event on node 3 must create a new case despite later node 4 closure, open=%+v", openCases)
	}
	parties, err := bench.registry.ListParties(ctx, openCases[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, party := range parties {
		if party.Kind == model.PartyExit && party.NodeID != 3 {
			t.Fatalf("new case must retain the event baseline exit, parties=%+v", parties)
		}
	}
}

func TestSimpleCourtDoesNotClearCooldownWhileAnotherCaseHoldsAccount(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	ctx := context.Background()

	if err := reportTestIncident(service, ctx, 7, model.EpochKey{NodeID: 3, Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	if err := reportTestIncident(service, ctx, 7, model.EpochKey{NodeID: 4, Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	cases, err := bench.registry.ListOpenCases(ctx)
	if err != nil || len(cases) != 2 {
		t.Fatalf("expected two cases, cases=%d err=%v", len(cases), err)
	}
	ordered := make(map[uint64]model.CaseRecord)
	for _, record := range cases {
		parties, listErr := bench.registry.ListParties(ctx, record.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, party := range parties {
			if party.Kind == model.PartyExit {
				ordered[party.NodeID] = record
			}
		}
	}
	for _, nodeID := range []uint64{3, 4} {
		record, ok := ordered[nodeID]
		if !ok {
			t.Fatalf("case for node %d not found", nodeID)
		}
		parties, listErr := bench.registry.ListParties(ctx, record.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if err := settleTestInsufficient(service, ctx, record, parties, "test_release", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if nodeID == 3 {
			accountState := bench.registry.AccountState(7)
			if accountState.State != model.AccountRemanded {
				t.Fatal("account must remain held by the second case")
			}
			if accountState.CurrentCaseID != ordered[4].ID {
				t.Fatalf("account explanatory case must move to surviving holder: state=%+v", accountState)
			}
		}
	}
	if bench.registry.AccountState(7).State != model.AccountActive {
		t.Fatalf("last case release must clear cooldown exactly once: state=%s", bench.registry.AccountState(7).State)
	}
}

func TestSimpleCourtDelayedReporterDoesNotReopenSettledIncident(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	ctx := context.Background()
	exit := model.EpochKey{NodeID: 3, Epoch: 0}

	if err := service.ReportDegradedAt(ctx, 7, exit, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cases, err := bench.registry.ListOpenCases(ctx)
	if err != nil || len(cases) != 1 {
		t.Fatalf("expected one initial case, cases=%d err=%v", len(cases), err)
	}
	caseID := cases[0].ID
	settleSimpleCaseUntilClosed(t, service, bench.registry, caseID, func(model.ProbeTaskView) model.ProbeTaskResult {
		return model.ProbeTaskResult{Outcome: model.ProbeResultError, Detail: "delayed_report_test"}
	})
	closed, found, err := bench.registry.GetCase(ctx, caseID)
	if err != nil || !found || !closed.Status.Closed() {
		t.Fatalf("initial incident must be closed before delayed report: case=%+v found=%v err=%v", closed, found, err)
	}

	// The request reporter can run after the periodic fallback. Its original
	// event time is before the closure and must therefore be idempotent.
	if err := service.ReportDegradedAt(ctx, 7, exit, closed.OpenedAt); err != nil {
		t.Fatal(err)
	}
	open, err := bench.registry.ListOpenCases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("delayed report must not reopen a settled incident, open=%+v", open)
	}
}

func TestSimpleCourtReleaseRespectsAnotherCaseHoldingExit(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := reportTestIncident(service, context.Background(), 7, model.EpochKey{NodeID: 3, Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	if err := reportTestIncident(service, context.Background(), 8, model.EpochKey{NodeID: 3, Epoch: 0}); err != nil {
		t.Fatal(err)
	}
	cases, err := bench.registry.ListOpenCases(context.Background())
	if err != nil || len(cases) != 2 {
		t.Fatalf("different defendants must have separate cases: cases=%d err=%v", len(cases), err)
	}
	var first model.CaseRecord
	for _, record := range cases {
		parties, listErr := bench.registry.ListParties(context.Background(), record.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, party := range parties {
			if party.Kind == model.PartyAccount && party.AccountID == 7 {
				first = record
			}
		}
	}
	if first.ID == 0 {
		t.Fatal("case for first defendant not found")
	}
	parties, err := bench.registry.ListParties(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := settleTestInsufficient(service, context.Background(), first, parties, "test_release", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if bench.registry.ExitStateOfCurrentEpoch(3).State != model.ExitRemanded {
		t.Fatal("one case releasing must not free an exit still held by another case")
	}
	if !bench.registry.AccountEligible(7) {
		t.Fatal("released defendant must become schedulable")
	}
}

func TestSimpleCourtPoolExitConvictionKeepsEpochHoldUntilChange(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	caseID := openSimpleTestCase(t, service, bench.registry)
	if err := bench.registry.DB().Exec("UPDATE egress_nodes SET proxy_pool = 1 WHERE id = 3").Error; err != nil {
		t.Fatal(err)
	}

	settleSimpleTestTasks(t, bench.registry, caseID, func(task model.ProbeTaskView) model.ProbeTaskResult {
		if task.Direction == model.ProbeExitJury {
			return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded}
		}
		return model.ProbeTaskResult{Outcome: model.ProbeResultClean}
	})
	if _, err := service.Evaluate(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if bench.registry.ExitStateOfCurrentEpoch(3).State != model.ExitRemanded {
		t.Fatal("pool exit conviction must wait for an epoch change instead of creating a permanent ban")
	}
	if !bench.registry.AccountEligible(7) {
		t.Fatal("pool exit conviction must release the defendant account")
	}
	records, err := bench.registry.ListRecentCases(context.Background(), 10)
	if err != nil || len(records) != 1 || records[0].Verdict != model.VerdictExitGuilty {
		t.Fatalf("pool exit conviction must still close the case: records=%+v err=%v", records, err)
	}
}

func TestSimpleCourtMarksPoolPartyWithdrawnAfterEpochChange(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	caseID := openSimpleTestCase(t, service, bench.registry)
	if err := bench.registry.DB().Exec("UPDATE egress_nodes SET proxy_pool = 1 WHERE id = 3").Error; err != nil {
		t.Fatal(err)
	}

	settleSimpleTestTasks(t, bench.registry, caseID, func(task model.ProbeTaskView) model.ProbeTaskResult {
		if task.Direction == model.ProbeExitJury {
			return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded}
		}
		return model.ProbeTaskResult{Outcome: model.ProbeResultClean}
	})
	if _, err := service.Evaluate(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bench.registry.AdvanceEpoch(context.Background(), 3, model.ExitIdentityFromAggregate("198.51.100.8")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Evaluate(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	parties, err := bench.registry.ListParties(context.Background(), caseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, party := range parties {
		if party.Kind == model.PartyExit && party.Disposition != model.DispositionWithdrawn {
			t.Fatalf("old pool epoch party must be withdrawn, got %+v", party)
		}
	}
}

func TestSimpleCourtPreservesWithdrawnExitHistoryWhenRoundSettles(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	caseID := openSimpleTestCase(t, service, bench.registry)

	if _, _, err := bench.registry.AdvanceEpoch(context.Background(), 3, model.ExitIdentityFromAggregate("changed")); err != nil {
		t.Fatal(err)
	}
	settleSimpleTestTasks(t, bench.registry, caseID, func(model.ProbeTaskView) model.ProbeTaskResult {
		return model.ProbeTaskResult{Outcome: model.ProbeResultError, Detail: "epoch_changed"}
	})
	if _, err := service.Evaluate(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	parties, err := bench.registry.ListParties(context.Background(), caseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, party := range parties {
		if party.Kind == model.PartyExit && party.Disposition != model.DispositionWithdrawn {
			t.Fatalf("epoch transition history must remain withdrawn after settlement, got %+v", party)
		}
	}
	if !bench.registry.AccountEligible(7) {
		// The defendant is released; preserving the historical exit disposition
		// must not retain the account hold.
		t.Fatal("insufficient epoch evidence must release the defendant account")
	}
}

func TestSimpleCourtLiveViewWaitingReasonCodes(t *testing.T) {
	bench := newBench(t)
	store := registry.NewProbeTaskStore(bench.registry)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: store})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	openSimpleTestCase(t, service, bench.registry)

	views, err := service.LiveCaseViews(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("expected one live view, got %d", len(views))
	}
	if views[0].WaitingReasonCode != "awaiting_probes" {
		t.Fatalf("fresh case with pending tasks must report awaiting_probes, got %q", views[0].WaitingReasonCode)
	}
	if views[0].DeadlineAt.IsZero() || views[0].Expired {
		t.Fatalf("fresh case must expose a future deadline: %+v", views[0])
	}

	// Past the deadline the code flips even while probes are still pending:
	// the panel must tell the operator the case is closing, not still waiting.
	expired, err := service.LiveCaseViews(context.Background(), time.Now().UTC().Add(2*cfg.InvestigationTimeout))
	if err != nil {
		t.Fatal(err)
	}
	if expired[0].WaitingReasonCode != "deadline_reached" {
		t.Fatalf("expired case must report deadline_reached, got %q", expired[0].WaitingReasonCode)
	}
}

func settleTestInsufficient(s *Service, ctx context.Context, record model.CaseRecord, parties []model.PartyRecord, reason string, now time.Time) error {
	return s.registry.SettleInvestigation(ctx, record.ID, model.VerdictInsufficient, `{"reason":"test_release"}`, now, true)
}
