package relational

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

func TestCacheUsagePresenceMigrationAndRoundTrip(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repoA, repoB := NewAuditRepository(a), NewAuditRepository(b)
			value := audit.Record{EventID: "evt_fictional_cache_legacy", RequestID: "fictional-cache", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, InputTokens: 256, CreatedAt: time.Now().UTC(), GenerationUsages: generationUsageFixture()}
			if err := repoA.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			// Native DROP COLUMN reconstructs the old nullable-column schema
			// without GORM's SQLite table recreation cascading into child rows.
			for _, table := range []string{"request_audits", "request_audit_generations"} {
				if err := a.db.Exec("ALTER TABLE " + table + " DROP COLUMN cached_input_tokens_reported").Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			old, err := repoB.Get(ctx, 1)
			if err != nil || old.CachedInputTokensReported != nil || old.InputTokens != 256 || !reflect.DeepEqual(old.GenerationUsages, value.GenerationUsages) {
				t.Fatalf("migration fabricated or lost historical facts: %+v %v", old, err)
			}
			present, absent := true, false
			for i, reported := range []*bool{nil, &absent, &present} {
				value.EventID = fmt.Sprintf("evt_fictional_cache_%04d", i)
				value.CachedInputTokensReported = reported
				for j := range value.GenerationUsages {
					value.GenerationUsages[j].PhysicalID = fmt.Sprintf("fictional-cache-%d/%d", i, j+1)
					value.GenerationUsages[j].CachedInputTokens = 0
					value.GenerationUsages[j].CachedInputTokensReported = reported
				}
				if err := repoA.Create(ctx, value); err != nil {
					t.Fatal(err)
				}
				if err := repoB.Create(ctx, value); err != nil {
					t.Fatal(err)
				}
				var row requestAuditModel
				if err := b.db.Where("event_id = ?", value.EventID).First(&row).Error; err != nil {
					t.Fatal(err)
				}
				got, err := repoB.Get(ctx, row.ID)
				if err != nil || !reflect.DeepEqual(got.CachedInputTokensReported, reported) || !reflect.DeepEqual(got.GenerationUsages, value.GenerationUsages) {
					t.Fatalf("cache presence round trip: %+v %v", got, err)
				}
			}
			if n := tableRowCount(t, b, "request_audit_generations"); n != 8 {
				t.Fatalf("duplicate details: %d", n)
			}
		})
	}
}
