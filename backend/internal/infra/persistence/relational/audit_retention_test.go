package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// insertRetentionAudit 插入一条最小合法审计（含一条 attempt 明细），
// created_at 由参数指定（绕过默认 now 语义）。
func insertRetentionAudit(t *testing.T, repo *AuditRepository, db *Database, createdAt time.Time, attemptStage string) uint64 {
	t.Helper()
	record := audit.Record{
		RequestID:   "retention-" + createdAt.Format("150405.000000000") + "-" + attemptStage,
		ClientKeyID: 1, ClientKeyName: "retention-key", ClientIP: "127.0.0.1",
		ModelRouteID: 1, ModelPublicID: "grok-4.6", ModelUpstreamModel: "Build/grok-4.6",
		Provider: "grok_build", Operation: "responses", StatusCode: 200, Streaming: true,
		CreatedAt: createdAt,
	}
	record.Attempts = []audit.Attempt{{
		Number: 1, Source: "upstream_http", Stage: attemptStage, Method: "POST", RequestPath: "/v1/responses",
		UpstreamStatusCode: func() *int { v := 200; return &v }(), StartedAt: createdAt,
	}}
	ctx := context.Background()
	if err := repo.Create(ctx, record); err != nil {
		t.Fatalf("插入审计失败: %v", err)
	}
	var id uint64
	if err := db.db.WithContext(ctx).Raw("SELECT id FROM request_audits WHERE request_id = ?", record.RequestID).Scan(&id).Error; err != nil || id == 0 {
		t.Fatalf("回查审计 id 失败: id=%d err=%v", id, err)
	}
	return id
}

func countRows(t *testing.T, db *Database, table string) int {
	t.Helper()
	var n int
	if err := db.db.Raw("SELECT count(*) FROM " + table).Scan(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestAuditRepositoryDeleteOlderThan 锁定审计保留清理语义：
// 只删 cutoff 之前的审计；对应 attempts 明细级联删除；新记录保留；
// limit 分批（返回值=本批删除数，<limit 表示排空）。
func TestAuditRepositoryDeleteOlderThan(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAuditRepository(database)

	now := time.Now().UTC()
	old1 := insertRetentionAudit(t, repo, database, now.Add(-72*time.Hour), "old-1")
	old2 := insertRetentionAudit(t, repo, database, now.Add(-48*time.Hour), "old-2")
	fresh := insertRetentionAudit(t, repo, database, now.Add(-time.Hour), "fresh")

	cutoff := now.Add(-24 * time.Hour)

	// limit=1 分批：第一批只删最旧一条（id 升序）。
	deleted, err := repo.DeleteOlderThan(ctx, cutoff, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("第一批 deleted=%d err=%v，应为 1", deleted, err)
	}
	if n := countRows(t, database, "request_audit_attempts"); n != 2 {
		t.Fatalf("第一批后 attempts=%d，应为 2（old-2+fresh）", n)
	}
	// 排空剩余旧记录。
	deleted, err = repo.DeleteOlderThan(ctx, cutoff, 500)
	if err != nil || deleted != 1 {
		t.Fatalf("第二批 deleted=%d err=%v，应为 1", deleted, err)
	}
	// 已排空：再跑返回 0。
	if deleted, err = repo.DeleteOlderThan(ctx, cutoff, 500); err != nil || deleted != 0 {
		t.Fatalf("排空后 deleted=%d err=%v，应为 0", deleted, err)
	}

	if n := countRows(t, database, "request_audits"); n != 1 {
		t.Fatalf("清理后审计数=%d，应只剩 fresh", n)
	}
	if n := countRows(t, database, "request_audit_attempts"); n != 1 {
		t.Fatalf("清理后 attempts=%d，应只剩 fresh 的明细", n)
	}
	// 孤儿检查：剩余 attempt 必须属于 fresh。
	var auditID uint64
	if err := database.db.Raw("SELECT audit_id FROM request_audit_attempts").Scan(&auditID).Error; err != nil || auditID != fresh {
		t.Fatalf("剩余 attempt 归属 audit_id=%d，应为 fresh=%d（孤儿检查）", auditID, fresh)
	}
	_ = old1
	_ = old2
}
