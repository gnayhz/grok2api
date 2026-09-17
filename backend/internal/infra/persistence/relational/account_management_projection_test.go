package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

func TestAccountQualityFilterAndIdentityHTTPAcrossInstances(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			writer, reader, db := qualityManagementPair(t, dialect)
			ctx := context.Background()
			repo := NewAccountRepository(db)
			normal := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: account.ProviderBuild, Name: "Synthetic normal", Email: "normal@example.invalid", SourceKey: "synthetic-normal"})
			held := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: account.ProviderBuild, Name: "Synthetic held", Email: "held@example.invalid", SourceKey: "synthetic-held", RiskStatus: "rsc_denied"})
			sentenced := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: account.ProviderConsole, Name: "Synthetic sentenced", Email: "sentenced@example.invalid", SourceKey: "synthetic-sentenced"})
			service := accountapp.NewService(repo, NewAuditRepository(db), nil, nil, nil, nil, nil, nil, nil, nil)
			service.SetQualityStates(func(ctx context.Context) (map[uint64]accountapp.QualityState, error) {
				state, err := reader.ManagementState(ctx)
				if err != nil {
					return nil, err
				}
				result := make(map[uint64]accountapp.QualityState, len(state.Accounts))
				for id, entry := range state.Accounts {
					result[id] = accountapp.QualityState{State: string(entry.State), CaseID: entry.CurrentCaseID}
				}
				return result, nil
			})
			router := gin.New()
			accounthttp.NewHandler(accounthttp.Dependencies{Administration: service}).Register(router.Group("/api/admin/v1"))
			request := func(path string, status int) map[string]json.RawMessage {
				t.Helper()
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/admin/v1/accounts"+path, nil))
				if recorder.Code != status {
					t.Fatalf("GET %s: %d %s", path, recorder.Code, recorder.Body.String())
				}
				var result struct {
					Data map[string]json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				return result.Data
			}
			count := func(query string, want int) {
				t.Helper()
				data := request("?page=1&pageSize=1&"+query, 200)
				var total int
				if err := json.Unmarshal(data["total"], &total); err != nil {
					t.Fatal(err)
				}
				if total != want {
					t.Fatalf("%s total=%d want=%d", query, total, want)
				}
				var items []struct {
					ID      string                           `json:"id"`
					Quality *accounthttp.AccountQualityState `json:"quality"`
				}
				if err := json.Unmarshal(data["items"], &items); err != nil {
					t.Fatal(err)
				}
				if len(items) > 1 {
					t.Fatal("filter bypassed pagination")
				}
				if want > 0 && query == "quality=restricted" && items[0].Quality == nil {
					t.Fatal("filter and badge disagree")
				}
			}
			count("quality=restricted", 0)
			first, err := writer.OpenInvestigation(ctx, held.ID, qualitymodel.EpochKey{NodeID: 701}, time.Now().UTC(), "{}")
			if err != nil {
				t.Fatal(err)
			}
			second, err := writer.OpenInvestigation(ctx, sentenced.ID, qualitymodel.EpochKey{NodeID: 702}, time.Now().UTC(), "{}")
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.TransitionAccount(ctx, qualitymodel.AccountTransitionRequest{AccountID: sentenced.ID, To: qualitymodel.AccountSentenced, CaseID: second}); err != nil {
				t.Fatal(err)
			}
			count("quality=restricted", 2)
			count("quality=remanded", 1)
			count("quality=sentenced", 1)
			count("quality=clear", 1)
			count("quality=restricted&risk=flagged", 1)
			count("quality=clear&risk=flagged", 0)
			count("quality=sentenced&provider=grok_build", 0)
			count("quality=restricted&search=Synthetic", 2)
			count("quality=restricted&search=%23"+strconv.FormatUint(normal.ID, 10), 0)
			request("?quality=invalid", 400)
			// The account may be deleted while its historical case and restriction remain.
			if err := repo.Delete(ctx, sentenced.ID); err != nil {
				t.Fatal(err)
			}
			count("quality=sentenced", 0)
			count("quality=restricted", 1)
			ids := fmt.Sprintf("/identities?ids=%d,%d,%d,999999", normal.ID, held.ID, sentenced.ID)
			data := request(ids, 200)
			var identities []map[string]json.RawMessage
			if err := json.Unmarshal(data["items"], &identities); err != nil {
				t.Fatal(err)
			}
			if len(identities) != 2 {
				t.Fatalf("deleted or missing references survived: %v", identities)
			}
			for _, identity := range identities {
				if len(identity) != 4 || identity["id"] == nil || identity["name"] == nil || identity["email"] == nil || identity["provider"] == nil {
					t.Fatalf("non-identity data escaped: %v", identity)
				}
			}
			request("/identities?ids=0", 400)
			request("/identities?ids=1,garbage", 400)
			service.SetQualityStates(func(context.Context) (map[uint64]accountapp.QualityState, error) {
				return nil, errors.New("synthetic projection unavailable")
			})
			request("?quality=clear", 500)
			// Preserve the quality history throughout account deletion and reads.
			if _, ok, err := writer.GetCase(ctx, first); err != nil || !ok {
				t.Fatal(err)
			}
			if _, ok, err := writer.GetCase(ctx, second); err != nil || !ok {
				t.Fatal(err)
			}
		})
	}
}

func TestLargeAccountRestrictionSetsStayWithinDriverLimits(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			_, _, db := qualityManagementPair(t, dialect)
			ctx := context.Background()
			repo := NewAccountRepository(db)
			value := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: account.ProviderBuild, Name: "Synthetic large pool", SourceKey: "synthetic-large"})
			ids := make([]uint64, 70000)
			for i := range ids {
				ids[i] = uint64(i) + 1000000
			}
			ids[0] = value.ID
			assertAccountFilterCount(t, ctx, repo, repository.AccountListFilter{AccountIDs: ids, RestrictIDs: true, Now: time.Now()}, 1)
			assertAccountFilterCount(t, ctx, repo, repository.AccountListFilter{ExcludeIDs: ids, Now: time.Now()}, 0)
		})
	}
}
