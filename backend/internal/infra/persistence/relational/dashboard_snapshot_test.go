package relational

import (
	"context"
	"strings"
	"testing"
	"time"

	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	"gorm.io/gorm"
)

// An audit commits through an independent pool between the overall usage read
// and the next aggregate. All projections in one response must keep the same
// database snapshot; the next request must see the new committed row.
func TestDashboardSnapshotKeepsOneCommittedView(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			now := time.Now().UTC()
			seed := requestAuditModel{RequestID: "before", ClientKeyID: 1, ModelRouteID: 1,
				Provider: "grok_build", ModelPublicID: "model", Operation: "responses",
				UsageSource: "upstream", StatusCode: 200, InputTokens: 10, OutputTokens: 5,
				TotalTokens: 15, CostInUSDTicks: 20, CreatedAt: now.Add(-time.Minute)}
			if err := db.db.WithContext(ctx).Create(&seed).Error; err != nil {
				t.Fatal(err)
			}
			inserted := make(chan error, 1)
			interleaved := false
			const callback = "g03_insert_between_dashboard_aggregates"
			if err := db.db.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
				if interleaved || tx.Statement.Table != "request_audits" || !strings.Contains(strings.Join(tx.Statement.Selects, " "), "quality_degraded_requests") {
					return
				}
				interleaved = true
				go func() {
					row := seed
					row.ID = 0
					row.RequestID = "between"
					row.StatusCode = 502
					row.ErrorCode = "quality_degraded"
					inserted <- peer.db.WithContext(ctx).Create(&row).Error
				}()
				if dialect == "postgres" {
					// PostgreSQL permits this concurrent write. SQLite's BEGIN
					// IMMEDIATE serializes it until the current snapshot finishes.
					select {
					case err := <-inserted:
						if err != nil {
							tx.AddError(err)
						}
						inserted <- err
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.db.Callback().Row().Remove(callback) })
			repo := NewDashboardRepository(db)
			window := testDashboardWindow(testDashboardBoundaries(now.Add(-time.Hour), time.Hour, 2))
			before, err := repo.Snapshot(ctx, window, now)
			if err != nil {
				t.Fatal(err)
			}
			if !interleaved {
				t.Fatal("concurrent write did not cross the aggregate boundary")
			}
			select {
			case err := <-inserted:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			checkDashboardCommittedView(t, before, 1, 0)
			after, err := repo.Snapshot(ctx, window, now)
			if err != nil {
				t.Fatal(err)
			}
			checkDashboardCommittedView(t, after, 2, 1)
			canceled, stop := context.WithCancel(ctx)
			stop()
			if _, err := repo.Snapshot(canceled, window, now); err == nil {
				t.Fatal("canceled snapshot reported success")
			}
		})
	}
}

func checkDashboardCommittedView(t *testing.T, snapshot dashboarddomain.Aggregate, requests, degraded int64) {
	t.Helper()
	var bucketRequests, bucketTokens, bucketCost, activityRequests, modelRequests, providerRequests int64
	for _, bucket := range snapshot.Buckets {
		bucketRequests += bucket.Requests
		bucketTokens += bucket.Tokens
		bucketCost += bucket.BilledCostUSDTicks
	}
	for _, bucket := range snapshot.ActivityBuckets {
		activityRequests += bucket.Requests
	}
	for _, model := range snapshot.TopModels {
		modelRequests += model.Requests
	}
	for _, provider := range snapshot.Providers {
		providerRequests += provider.Requests
	}
	if snapshot.Usage.Requests != requests || bucketRequests != requests || activityRequests != requests || modelRequests != requests || providerRequests != requests || snapshot.Resources.QualityDegradedRequests != degraded {
		t.Errorf("mixed dashboard view: expected=%d degraded=%d, usage=%d buckets=%d activity=%d models=%d providers=%d degraded=%d", requests, degraded, snapshot.Usage.Requests, bucketRequests, activityRequests, modelRequests, providerRequests, snapshot.Resources.QualityDegradedRequests)
	}
	if snapshot.Usage.Tokens != requests*15 || bucketTokens != requests*15 || snapshot.Usage.BilledCostUSDTicks != requests*20 || bucketCost != requests*20 {
		t.Errorf("mixed dashboard totals: expected=%d usage=%+v bucket_tokens=%d bucket_cost=%d", requests, snapshot.Usage, bucketTokens, bucketCost)
	}
}
