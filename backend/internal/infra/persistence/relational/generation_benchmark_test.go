package relational

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// The identical source also compiles on D05: older records ignore the new
// JSON field. Reflection only supplies unique physical IDs when supported.
// This isolates SQL persistence cost; it is not an end-to-end throughput test.
func BenchmarkAuditKnownGenerationPersistence(b *testing.B) {
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "generation.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	var value audit.Record
	if err := json.Unmarshal([]byte(`{"ClientKeyID":1,"ModelRouteID":1,"StatusCode":200,"InputTokens":20,"OutputTokens":5,"TotalTokens":25,"GenerationUsages":[{"physical_id":"bench/1","ordinal":1,"account_id":1,"account_name":"bench","model":"grok-4.5","selected":true,"outcome":"completed","usage_source":"upstream","input_tokens":20,"output_tokens":5,"total_tokens":25}]}`), &value); err != nil {
		b.Fatal(err)
	}
	field := reflect.ValueOf(&value).Elem().FieldByName("GenerationUsages")
	var physicalID reflect.Value
	if field.IsValid() && field.Len() > 0 {
		physicalID = field.Index(0).FieldByName("PhysicalID")
	}
	value.CreatedAt = time.Now().UTC()
	repo := NewAuditRepository(db)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value.EventID = fmt.Sprintf("evt_generation_bench_%012d", i)
		value.RequestID = value.EventID
		if physicalID.IsValid() {
			physicalID.SetString(value.EventID + "/1")
		}
		if err := repo.Create(ctx, value); err != nil {
			b.Fatal(err)
		}
	}
}
