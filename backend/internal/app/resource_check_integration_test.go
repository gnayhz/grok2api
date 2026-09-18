package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestResourceCheckApplicationCrossAttributionAndProgress(t *testing.T) {
	for _, scenario := range []string{"healthy-account", "bad-account", "bad-node"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/cdn-cgi/trace" {
					if r.Header.Get("Authorization") != "" {
						t.Error("credential leaked to trace")
					}
					fmt.Fprintf(w, "ip=198.51.100.%s\n", r.Header.Get("X-Fictional-Path"))
					return
				}
				if r.URL.Path != "/v1/responses" {
					http.NotFound(w, r)
					return
				}
				calls.Add(1)
				var body struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				bad := scenario == "bad-account" && r.Header.Get("Authorization") == "Bearer fictional-resource-0" || scenario == "bad-node" && r.Header.Get("X-Fictional-Path") == "1"
				count, delta := 100, 180
				if bad {
					count, delta = 110, 90
				}
				if strings.Count(body.Input, "a") > 1024 {
					count += delta
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"fictional-resource\",\"status\":\"in_progress\"}}\n\n")
				if !bad {
					fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"fictional thinking\"}\n\n")
				}
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fictional-resource\",\"status\":\"completed\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":10,\"output_tokens_details\":{\"reasoning_tokens\":8}}}}\n\n", count)
			}))
			defer upstream.Close()
			a := newLifecycleApplication(t, func(cfg *config.Config) {
				cfg.Provider.Build.BaseURL = upstream.URL + "/v1"
				cfg.Provider.Build.FallbackBaseURL = "disabled"
			})
			ctx := context.Background()
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			ids := []uint64{}
			for i := range 5 {
				token, err := cipher.Encrypt(fmt.Sprintf("fictional-resource-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				c, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: fmt.Sprintf("fictional-resource-%d", i), SourceKey: fmt.Sprintf("fictional-resource-%d", i), EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, c.ID)
				if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, c.ID, []string{"grok-4.6"}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewEgressRepository(a.database)
			nodes := []uint64{}
			for i := range 4 {
				target, _ := url.Parse(upstream.URL)
				reverse := httputil.NewSingleHostReverseProxy(target)
				director := reverse.Director
				reverse.Director = func(r *http.Request) { director(r); r.Header.Set("X-Fictional-Path", fmt.Sprint(i+1)) }
				server := httptest.NewServer(reverse)
				defer server.Close()
				encrypted, err := cipher.Encrypt(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				node, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: fmt.Sprintf("fictional-exit-%d", i), Enabled: true, Health: 1, EncryptedProxyURL: encrypted})
				if err != nil {
					t.Fatal(err)
				}
				nodes = append(nodes, node.ID)
				if _, _, err := a.quality.AdvanceEpoch(ctx, node.ID, model.ExitIdentity{IPv4: fmt.Sprintf("198.51.100.%d", i+1)}); err != nil {
					t.Fatal(err)
				}
			}
			_, err = a.quality.OpenInvestigation(ctx, ids[0], model.EpochKey{NodeID: nodes[0]}, time.Now().UTC(), `{}`)
			if err != nil {
				t.Fatal(err)
			}
			before := a.quality.AccountState(ids[0])
			store := registry.NewProbeTaskStore(a.quality)
			service := management.NewResourceChecks(store, a.gateway, baseNodeSource{egress: a.egressOps})
			kind, id := "account", ids[0]
			if scenario == "bad-node" {
				kind, id = "node", nodes[0]
			}
			items, err := service.Start(ctx, kind, []uint64{id}, "grok-4.6")
			if err != nil || len(items) != 1 || items[0].ID == 0 {
				t.Fatalf("submit %+v %v", items, err)
			}
			if err := a.qualityInvestigator.RunDueOnce(ctx, a.qualityProbeExec, 1); err != nil {
				t.Fatal(err)
			}
			rows, err := service.List(ctx, kind, []uint64{id})
			if err != nil || len(rows) != 1 || rows[0].Report == nil {
				t.Fatalf("report %+v %v", rows, err)
			}
			want, wantCalls := "degraded", 21
			if scenario == "healthy-account" {
				want, wantCalls = "healthy", 14
			}
			if rows[0].State != model.ProbeDone || rows[0].Report.Outcome != want || int(calls.Load()) != wantCalls {
				raw, _ := json.Marshal(rows[0])
				t.Fatalf("calls %d report %s", calls.Load(), raw)
			}
			if a.quality.AccountState(ids[0]).State != before.State {
				t.Fatal("check changed case restriction")
			}
		})
	}
}
