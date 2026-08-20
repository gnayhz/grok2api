package relational

import (
	"context"
	"testing"
)

// TestSchemaCreatesErrorCodeIndex locks in the errorCode index migration:
// the audit errorCode filter (quality_degraded preset) and the dashboard
// degrade-event count would otherwise scan every row in the time window via
// the created_at index alone. The index follows the house pattern
// (filter column, created_at DESC, id DESC) so it also serves cursor order.
func TestSchemaCreatesErrorCodeIndex(t *testing.T) {
	database := openTestDatabase(t)
	var count int
	if err := database.db.WithContext(context.Background()).
		Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_audits_error_code_created_id'").
		Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("idx_audits_error_code_created_id missing: errorCode filter would degrade to a window scan")
	}
}
