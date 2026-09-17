package clientkey

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func TestClientKeyListsReservedUsageSeparatelyFromSettlement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "billing-view.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	service := keyapp.NewService("contract-owner", relational.NewClientKeyRepository(db), nil, nil, 120, 8, cipher, security.RandomTokenSource{})
	created, err := service.Create(ctx, keyapp.CreateInput{Name: "pending-budget", Enabled: true, BillingLimitUSDTicks: 10000000000})
	if err != nil {
		t.Fatal(err)
	}
	eventID := "evt_http_reserved_budget"
	if ok, err := service.ReserveBilling(ctx, created.Key, eventID, 80000000, time.Hour); err != nil || !ok {
		t.Fatalf("reserve=%v %v", ok, err)
	}
	router := gin.New()
	NewHandler(service).Register(router.Group("/api/admin/v1"))
	read := func(billed, reserved int64) []byte {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/admin/v1/client-keys", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("list HTTP %d: %s", response.Code, response.Body.String())
		}
		var payload struct {
			Data struct {
				Items []struct {
					Billed   int64 `json:"billedUsageUsdTicks"`
					Reserved int64 `json:"reservedUsageUsdTicks"`
				}
			}
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Data.Items) != 1 || payload.Data.Items[0].Billed != billed || payload.Data.Items[0].Reserved != reserved {
			t.Fatalf("billing view hid or merged reservation: %+v", payload)
		}
		return append([]byte(nil), response.Body.Bytes()...)
	}
	pending := read(0, 80000000)
	if path := os.Getenv("TEST_CLIENT_KEY_BILLING_FIXTURE"); path != "" {
		if err := os.WriteFile(path, pending, 0600); err != nil {
			t.Fatal(err)
		}
	}
	value := audit.Record{EventID: eventID, RequestID: eventID, ClientKeyID: created.Key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 30000000, CreatedAt: time.Now().UTC()}
	if err := relational.NewAuditRepository(db).Create(ctx, value); err != nil {
		t.Fatal(err)
	}
	read(30000000, 0)
}
