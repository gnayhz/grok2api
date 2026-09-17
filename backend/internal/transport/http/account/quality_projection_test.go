package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/gin-gonic/gin"
)

type qualityDetailAccounts struct{ accountapp.Administration }

func (qualityDetailAccounts) Get(context.Context, uint64) (accountapp.View, error) {
	return accountapp.View{Credential: accountdomain.Credential{ID: 42}, Quality: &accountapp.QualityState{State: "remanded", CaseID: 7}}, nil
}

func TestAccountDetailIncludesInjectedQualityState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(Dependencies{Administration: qualityDetailAccounts{}})
	router := gin.New()
	handler.Register(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/admin/v1/accounts/42", nil))
	var payload struct {
		Data struct {
			Quality *AccountQualityState `json:"quality"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || payload.Data.Quality == nil || *payload.Data.Quality != (AccountQualityState{State: "remanded", CaseID: 7}) {
		t.Fatalf("detail lost quality projection: %d %s", recorder.Code, recorder.Body.String())
	}
}
