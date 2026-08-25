package adminauth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// TestRefreshReuseKillsTokenFamily：轮换后旧 token 重用的双语义。
// 宽限窗内（<30s）：良性重复刷新——旧 token 拒绝但抢先轮换者的新 token
// 存活（多标签页/重试竞速不登出）；超窗重用：窃取信号（OAuth BCP
// RFC 6819 §5.2.11）——吊销整个 token family，最新 token 一并死亡。
func TestRefreshReuseKillsTokenFamily(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "family.db")
	database, err := relational.OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 独立连接用于回拨 last_used_at（仓储刻意不暴露裸 SQL 面）。
	backdoor, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backdoor.Close()

	sessions := relational.NewAdminSessionRepository(database)
	service := NewService(relational.NewAdminRepository(database), sessions, security.NewTokenService("12345678901234567890123456789012"), time.Minute, time.Hour)
	ctx := context.Background()
	if err := service.Bootstrap(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	_, first, err := service.Login(ctx, "admin", "password123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := service.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}

	// 窗内重用：拒绝但家族存活（家族检查本身会把 T2 轮换为 T3'）。
	if _, err := service.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("in-window reuse must be rejected, got %v", err)
	}
	survived, err := service.Refresh(ctx, rotated.RefreshToken)
	if err != nil {
		t.Fatalf("in-window reuse must not kill the family: %v", err)
	}

	// 把该会话 last_used_at 回拨到宽限窗外，模拟窃取者轮换后受害者
	// 超窗呈上旧 token。
	backdate := time.Now().UTC().Add(-2 * refreshRotationGrace)
	if _, err := backdoor.Exec("UPDATE admin_sessions SET last_used_at = ? WHERE refresh_token_hash = ?", backdate, security.HashToken(survived.RefreshToken)); err != nil {
		t.Fatal(err)
	}

	// 超窗重用旧 token（T2）：必须吊销整个家族（最新 token 一并死亡）。
	if _, err := service.Refresh(ctx, rotated.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("late reuse must be rejected, got %v", err)
	}
	if _, err := service.Refresh(ctx, survived.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("late reuse must kill the family (newest token died): %v", err)
	}
	if _, err := sessions.GetByTokenHash(ctx, security.HashToken(survived.RefreshToken)); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("session row must be revoked on late reuse, got err=%v", err)
	}
}
