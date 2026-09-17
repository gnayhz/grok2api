package evidence

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	gormDB, err := openTestSQLite(t)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	store, err := New(context.Background(), gormDB, model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatalf("构建证据局: %v", err)
	}
	t.Cleanup(func() { _ = closeTestSQLite(gormDB) })
	return store
}

func obsAt(at time.Time, account uint64, node, epoch uint64, outcome model.Outcome) model.Observation {
	return model.Observation{At: at, AccountID: account, Exit: model.EpochKey{NodeID: node, Epoch: epoch}, Outcome: outcome, Source: model.SourceTraffic}
}

// TestRecordAndSnapshotStats 锚定基础聚合:可判定观测进统计。
func TestRecordAndSnapshotStats(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Minute)
	observations := []model.Observation{
		obsAt(base, 1, 10, 0, model.OutcomeDegraded),
		obsAt(base.Add(time.Second), 1, 10, 0, model.OutcomeDelivered),
		obsAt(base.Add(2*time.Second), 2, 10, 0, model.OutcomeDegraded),
		obsAt(base.Add(3*time.Second), 1, 11, 0, model.OutcomeDelivered),
	}
	for _, obs := range observations {
		if err := store.Record(ctx, obs); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := store.SnapshotWindow(time.Now().UTC())
	exit := snapshot.Exits[model.EpochKey{NodeID: 10, Epoch: 0}]
	if exit.N != 3 || exit.Degraded != 2 {
		t.Fatalf("出口聚合 = %+v", exit)
	}
	account := snapshot.Accounts[1]
	if account.N != 3 || account.Degraded != 1 {
		t.Fatalf("账号聚合 = %+v", account)
	}
	rate, nodes, accounts := snapshot.Incidence()
	if rate != 0.5 || nodes != 2 || accounts != 2 {
		t.Fatalf("发病率 = %v %d %d", rate, nodes, accounts)
	}
}

// TestErrorObservationsExcluded 锚定 I10:传输 error 同时剔除出
// 分子与分母——不可采证据。
func TestErrorObservationsExcluded(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 5; i++ {
		if err := store.Record(ctx, obsAt(base, 1, 10, 0, model.OutcomeError)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Record(ctx, obsAt(base, 1, 10, 0, model.OutcomeDegraded)); err != nil {
		t.Fatal(err)
	}
	snapshot := store.SnapshotWindow(time.Now().UTC())
	exit := snapshot.Exits[model.EpochKey{NodeID: 10, Epoch: 0}]
	if exit.N != 1 || exit.Degraded != 1 {
		t.Fatalf("error 必须剔除: %+v", exit)
	}
	total, _ := store.Count(ctx)
	if total != 6 {
		t.Fatalf("error 留档但不采: %d", total)
	}
}

// TestWindowTrim 锚定滑窗语义:窗口外观测过期。
func TestWindowTrim(t *testing.T) {
	cfg := model.DefaultEvidenceConfig()
	cfg.Window = 50 * time.Millisecond
	gormDB, err := openTestSQLite(t)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSQLite(gormDB)
	store, err := New(context.Background(), gormDB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Record(context.Background(), obsAt(now.Add(-time.Second), 1, 10, 0, model.OutcomeDegraded)); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), obsAt(now, 2, 10, 0, model.OutcomeDelivered)); err != nil {
		t.Fatal(err)
	}
	snapshot := store.SnapshotWindow(now)
	if len(snapshot.Accounts) != 1 || snapshot.Decidable != 1 {
		t.Fatalf("滑窗裁剪失败: %+v", snapshot)
	}
}

// TestWindowKeepsOutOfOrderObservations 锚定证据窗口顺序:
// 乱序到达的观测必须仍按事件时刻参与滑窗,不能因前缀裁剪丢掉较新的证据。
func TestWindowKeepsOutOfOrderObservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out-of-order.db")
	gormDB, err := openTestSQLiteAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSQLite(gormDB)
	store, err := New(context.Background(), gormDB, model.EvidenceConfig{Window: time.Minute, Retention: time.Hour, MinWitnessObs: 1})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	newObservation := func(at time.Time, account uint64) model.Observation {
		return model.Observation{
			At: at, AccountID: account, Exit: model.EpochKey{NodeID: 1},
			Source: model.SourceTraffic, Outcome: model.OutcomeDelivered, Rule: "test",
		}
	}
	if err := store.Record(context.Background(), newObservation(base.Add(10*time.Second), 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), newObservation(base, 2)); err != nil {
		t.Fatal(err)
	}
	snapshot := store.SnapshotWindow(base.Add(10 * time.Second))
	if snapshot.Decidable != 2 || len(snapshot.Accounts) != 2 {
		t.Fatalf("乱序观测不得丢失, decidable=%d accounts=%d", snapshot.Decidable, len(snapshot.Accounts))
	}

	if err := store.Record(context.Background(), newObservation(base.Add(2*time.Minute), 3)); err != nil {
		t.Fatal(err)
	}
	snapshot = store.SnapshotWindow(base.Add(2 * time.Minute))
	if snapshot.Decidable != 1 || len(snapshot.Accounts) != 1 {
		t.Fatalf("窗口应按最新事件裁剪, decidable=%d accounts=%d", snapshot.Decidable, len(snapshot.Accounts))
	}
}

// TestSetConfigExpandsWindowFromDatabase 锚定运行参数热应用:
// 统计窗口扩大后必须从 q_observation 找回原先已被短窗口裁掉的证据,
// 不能只有重启才能看到新窗口。
func TestSetConfigExpandsWindowFromDatabase(t *testing.T) {
	gormDB, err := openTestSQLite(t)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSQLite(gormDB)
	store, err := New(context.Background(), gormDB, model.EvidenceConfig{Window: 50 * time.Millisecond, Retention: time.Hour, MinWitnessObs: 1})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	if err := store.Record(context.Background(), obsAt(base, 1, 10, 0, model.OutcomeDelivered)); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), obsAt(base.Add(100*time.Millisecond), 2, 10, 0, model.OutcomeDelivered)); err != nil {
		t.Fatal(err)
	}
	if got := store.SnapshotWindow(base.Add(100 * time.Millisecond)).Decidable; got != 1 {
		t.Fatalf("短窗口前置裁剪应只保留最新观测, got %d", got)
	}
	store.SetConfig(model.EvidenceConfig{Window: time.Minute, Retention: time.Hour, MinWitnessObs: 1})
	if got := store.SnapshotWindow(base.Add(100 * time.Millisecond)).Decidable; got != 2 {
		t.Fatalf("扩大窗口后应从库恢复旧观测, got %d", got)
	}
}

// TestSetConfigKeepsPreviousConfigWhenWindowReloadFails 锚定热应用失败
// 语义:扩大窗口需要读库回载，读库失败时配置与内存窗口都必须保持旧值，
// 不能让管理面显示新参数而法院继续使用旧口径。
func TestSetConfigKeepsPreviousConfigWhenWindowReloadFails(t *testing.T) {
	gormDB, err := openTestSQLite(t)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSQLite(gormDB)
	store, err := New(context.Background(), gormDB, model.EvidenceConfig{Window: time.Minute, Retention: time.Hour, MinWitnessObs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := gormDB.Exec("DROP TABLE q_observation").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfig(model.EvidenceConfig{Window: 2 * time.Minute, Retention: 2 * time.Hour, MinWitnessObs: 2}); err == nil {
		t.Fatal("窗口回载失败时必须返回错误")
	}
	got := store.Config()
	if got.Window != time.Minute || got.Retention != time.Hour || got.MinWitnessObs != 1 {
		t.Fatalf("回载失败不得部分应用配置: %+v", got)
	}
}

// TestRecordWaitsForWindowReloadCriticalSection 锚定证据一致性:
// 窗口回载期间，新的观测必须等待“落库+入窗”临界区完成，不能在回载
// 替换后丢失。
func TestRecordWaitsForWindowReloadCriticalSection(t *testing.T) {
	store := openTestStore(t)
	store.reloadMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- store.Record(context.Background(), obsAt(time.Now().UTC(), 7, 10, 0, model.OutcomeDelivered))
	}()
	select {
	case err := <-done:
		t.Fatalf("Record 不应绕过窗口回载临界区, err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}
	store.reloadMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := store.SnapshotWindow(time.Now().UTC()).Decidable; got != 1 {
		t.Fatalf("临界区释放后已提交观测必须立即可见, got %d", got)
	}
}

// TestStoreRestartRebuildsWindow 锚定 I17(证据侧):库为真相源,
// 重启回填滑窗。
func TestStoreRestartRebuildsWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.db")
	gormDB, err := openTestSQLiteAt(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(context.Background(), gormDB, model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Record(context.Background(), obsAt(time.Now().UTC(), 5, 10, 0, model.OutcomeDegraded)); err != nil {
		t.Fatal(err)
	}
	if err := closeTestSQLite(gormDB); err != nil {
		t.Fatal(err)
	}
	secondDB, err := openTestSQLiteAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSQLite(secondDB)
	second, err := New(context.Background(), secondDB, model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := second.SnapshotWindow(time.Now().UTC())
	if snapshot.Decidable != 1 || snapshot.Degraded != 1 {
		t.Fatalf("重启回填失败: %+v", snapshot)
	}
}

// TestCleanExpired 锚定 B3 决议1:保留期滚动清理。
func TestCleanExpired(t *testing.T) {
	cfg := model.DefaultEvidenceConfig()
	cfg.Retention = time.Hour
	gormDB, err := openTestSQLite(t)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSQLite(gormDB)
	store, err := New(context.Background(), gormDB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.Record(ctx, obsAt(now.Add(-2*time.Hour), 1, 10, 0, model.OutcomeDegraded)); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(ctx, obsAt(now, 2, 10, 0, model.OutcomeDegraded)); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanExpired(ctx, now); err != nil {
		t.Fatal(err)
	}
	count, _ := store.Count(ctx)
	if count != 1 {
		t.Fatalf("保留期清理后应剩 1 条, got %d", count)
	}
}

// TestObservationSchemaCarriesNoPlaintextAddress 锚定 I24(证据侧):
// 证据观测表列集合不含 IP 明文/账号名列(出口按 node+epoch 键控)。
func TestObservationSchemaCarriesNoPlaintextAddress(t *testing.T) {
	store := openTestStore(t)
	columns, err := store.db.Migrator().ColumnTypes(&ObservationModel{})
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{"ip": true, "address": true, "account_name": true, "username": true}
	for _, column := range columns {
		if forbidden[column.Name()] {
			t.Fatalf("证据表不得含明文地址列: %s", column.Name())
		}
	}
}

func TestSubmissionIdentitySurvivesEvidenceRestart(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC()
	fact := obsAt(now, 17, 23, 11, model.OutcomeDelivered)
	fact.Attempt = attemptmeta.Identity{ID: "synthetic/1", RequestID: "synthetic", AccountID: 17, Provider: "grok_build", Model: "synthetic-model", Revision: 4, RuleVersion: "test", StartedAt: now.Add(-time.Second), Path: attemptmeta.Path{NodeID: 23, Epoch: 11, Status: attemptmeta.PathRegistered}}
	if err := store.Record(context.Background(), fact); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), store.db, model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	events := restarted.window.events
	if len(events) != 1 || events[0].Attempt.ID != fact.Attempt.ID || events[0].Attempt.Revision != 4 || events[0].Attempt.Path.Epoch != 11 || !events[0].At.Equal(now) {
		t.Fatalf("restart changed evidence identity: %+v", events)
	}
}

func TestSettingsAndPeriodicRefreshKeepOneWindow(t *testing.T) {
	db, err := openTestSQLite(t)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSQLite(db)
	store, err := New(context.Background(), db, model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for worker := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 40 {
				var err error
				if worker == 0 {
					err = store.RefreshWindow(context.Background())
				} else {
					cfg := model.DefaultEvidenceConfig()
					cfg.Window = time.Duration(1+worker+round%3) * time.Hour
					err = store.SetConfigContext(context.Background(), cfg)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	store.window.mu.Lock()
	window := store.window.window
	store.window.mu.Unlock()
	if window != store.Config().Window {
		t.Fatalf("periodic refresh restored stale window: runtime=%v config=%v", window, store.Config().Window)
	}
	before := store.Config()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := before
	cfg.Window *= 2
	if err := store.SetConfigContext(ctx, cfg); err == nil || store.Config().Window != before.Window {
		t.Fatal("canceled config changed evidence window")
	}
}
