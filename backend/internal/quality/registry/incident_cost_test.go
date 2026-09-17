package registry

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestIncidentQueriesStayIndexedWithHistory(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			r := openTestRegistry(t)
			ctx := context.Background()
			if driver == "postgres" {
				opts, _ := isolatedQualityPostgres(t)
				var err error
				r, err = Open(ctx, opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
			}
			size := 2000
			if n, _ := strconv.Atoi(os.Getenv("GROK_REVIEW_HISTORY_ROWS")); n > size {
				size = n
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			for i, sql := range []string{
				`WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x < ?) INSERT INTO q_case(id,status,verdict,evidence_json,opened_at,closed_at,updated_at) SELECT x,'dismissed','insufficient','{}',?,?,? FROM seq`,
				`WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x < ?) INSERT INTO q_case_party(case_id,kind,account_id,node_id,epoch,role,disposition,updated_at) SELECT x,'account',x,0,0,'defendant','released',? FROM seq`,
				`WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x < ?) INSERT INTO q_case_party(case_id,kind,account_id,node_id,epoch,role,disposition,updated_at) SELECT x,'exit',0,5,x,'co_remanded','released',? FROM seq`,
			} {
				args := []any{size, now}
				if i == 0 {
					args = []any{size, now, now, now}
				}
				if err := r.DB().Exec(sql, args...).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := recordIncidentClosures(r.DB(), nil); err != nil {
				t.Fatal(err)
			}
			key := model.IncidentKey{AccountID: 1, Exit: model.EpochKey{NodeID: 5, Epoch: 1}}
			start := time.Now()
			got, err := r.LastClosedAtForIncidents(ctx, []model.IncidentKey{key, key, {AccountID: uint64(size + 1)}})
			if err != nil || len(got) != 1 || !got[key].Equal(now) {
				t.Fatalf("point lookup: %v %v", got, err)
			}
			t.Logf("history=%d point closure lookup=%v returned=%d", size, time.Since(start), len(got))
			start = time.Now()
			allowed, err := r.ExitAllowed(ctx, 5)
			if err != nil || !allowed {
				t.Fatalf("healthy admission: %v %v", allowed, err)
			}
			t.Logf("history=%d healthy admission=%v", size, time.Since(start))
			queries := []string{
				`SELECT 1 FROM q_case_party p JOIN q_case c ON c.id=p.case_id WHERE p.kind='exit' AND p.node_id=5 AND p.epoch=0 AND (p.disposition='sentenced' OR (p.disposition='remanded' AND c.status IN ('investigating','exit_guilty')))`,
				`SELECT * FROM q_incident_closure WHERE account_id=1 AND node_id=5 AND epoch=1`,
			}
			if driver == "postgres" {
				for _, table := range []string{"q_case_party", "q_incident_closure"} {
					if err := r.DB().Exec("ANALYZE " + table).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, query := range queries {
				var lines []string
				if driver == "sqlite" {
					var plans []struct{ Detail string }
					if err := r.DB().Raw("EXPLAIN QUERY PLAN " + query).Scan(&plans).Error; err != nil {
						t.Fatal(err)
					}
					for _, p := range plans {
						lines = append(lines, p.Detail)
					}
				} else {
					rows, err := r.DB().Raw("EXPLAIN " + query).Rows()
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var line string
						if err := rows.Scan(&line); err != nil {
							t.Fatal(err)
						}
						lines = append(lines, line)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					rows.Close()
				}
				plan := strings.Join(lines, "\n")
				t.Log(plan)
				if strings.Contains(plan, "Seq Scan") || strings.Contains(plan, "SCAN p") || strings.Contains(plan, "SCAN q_incident_closure") {
					t.Fatalf("historical scan returned:\n%s", plan)
				}
			}
			// Point closure reads do not access old case detail tables at all.
			if err := r.DB().Exec("ALTER TABLE q_case_party RENAME TO saved_case_party").Error; err != nil {
				t.Fatal(err)
			}
			if _, err := r.LastClosedAtForIncidents(ctx, []model.IncidentKey{key}); err != nil {
				t.Fatalf("lookup depended on case history: %v", err)
			}
			if err := r.DB().Exec("ALTER TABLE saved_case_party RENAME TO q_case_party").Error; err != nil {
				t.Fatal(err)
			}
		})
	}
}
