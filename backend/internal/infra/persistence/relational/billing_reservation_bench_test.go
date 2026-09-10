package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func BenchmarkBillingReservationSQLite(b *testing.B) {
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "reservation.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	keys := NewClientKeyRepository(db)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "bench", Prefix: "bench", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 100})
	if err != nil {
		b.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ok, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_bench_reservation", 80, expiry, repository.BillingReservationScope{OwnerID: "bench-owner"}); err != nil || !ok {
			b.Fatalf("reserve=%v %v", ok, err)
		}
		if err := keys.CancelBillingReservation(ctx, "evt_bench_reservation"); err != nil {
			b.Fatal(err)
		}
	}
}
