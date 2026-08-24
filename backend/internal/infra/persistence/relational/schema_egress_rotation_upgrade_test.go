package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// legacyEgressNodeWithoutRotation 复刻换 IP/降智列加入前的节点表结构。
type legacyEgressNodeWithoutRotation struct {
	ID                        uint64  `gorm:"primaryKey;autoIncrement"`
	Name                      string  `gorm:"size:160"`
	Enabled                   bool    `gorm:"not null;default:true"`
	ProxyPool                 bool    `gorm:"not null;default:false"`
	AccountCapacity           int     `gorm:"not null;default:0"`
	EncryptedProxyURL         string  `gorm:"type:text;not null;default:''"`
	UserAgent                 string  `gorm:"size:512;not null;default:''"`
	EncryptedCloudflareCookie string  `gorm:"type:text;not null;default:''"`
	Health                    float64 `gorm:"not null;default:1"`
	FailureCount              int     `gorm:"not null;default:0"`
	CooldownUntil             *time.Time
	LastError                 string `gorm:"size:512"`
	ProbeStatus               string `gorm:"size:16;not null;default:unknown"`
	LastProbedAt              *time.Time
	ProbeLatencyMS            int       `gorm:"not null;default:0"`
	ExitIP                    string    `gorm:"size:64;not null;default:''"`
	ProbeError                string    `gorm:"size:512;not null;default:''"`
	ProbeProvider             string    `gorm:"size:16;not null;default:''"`
	CreatedAt                 time.Time `gorm:"not null"`
	UpdatedAt                 time.Time `gorm:"not null"`
}

func (legacyEgressNodeWithoutRotation) TableName() string { return "egress_nodes" }

// 旧库升级：新增换 IP/降智列带默认值，已有行可读；窄方法可写可读。
func TestSchemaUpgradesEgressNodeRotationColumns(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-rotation-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := database.db.AutoMigrate(&legacyEgressNodeWithoutRotation{}); err != nil {
		t.Fatal(err)
	}
	legacy := legacyEgressNodeWithoutRotation{ID: 1, Name: "legacy", Enabled: true, Health: 1}
	if err := database.db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	var node egressNodeModel
	if err := database.db.First(&node, 1).Error; err != nil {
		t.Fatal(err)
	}
	if node.EncryptedRotationURL != "" || node.RotationAttempts != 0 || node.DegradeCount != 0 || node.LastRotatedAt != nil || node.LastDegradedAt != nil {
		t.Fatalf("upgraded legacy row has unexpected rotation state: %#v", node)
	}

	egressRepo := NewEgressRepository(database)
	until := time.Now().Add(time.Hour).UTC()
	degradedAt := time.Now().UTC()
	if err := egressRepo.UpdateEgressNodeQualityState(ctx, 1, 0.5, 2, &until, domain.LastErrorExitIPQuality, 1, &degradedAt); err != nil {
		t.Fatal(err)
	}
	rotatedAt := degradedAt.Add(time.Minute)
	if err := egressRepo.UpdateEgressNodeRotationState(ctx, 1, &rotatedAt, 1, "canary degraded"); err != nil {
		t.Fatal(err)
	}
	updated, err := egressRepo.GetEgressNode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastError != domain.LastErrorExitIPQuality || updated.DegradeCount != 1 || updated.RotationAttempts != 1 || updated.LastRotationError != "canary degraded" {
		t.Fatalf("rotation round-trip mismatch: %#v", updated)
	}
	if updated.CooldownUntil == nil || !updated.CooldownUntil.After(time.Now()) {
		t.Fatalf("cooldown not persisted: %#v", updated.CooldownUntil)
	}
	if updated.LastRotatedAt == nil || updated.LastDegradedAt == nil {
		t.Fatalf("timestamps not persisted")
	}

	// 未知节点必须返回 NotFound 而非静默成功。
	if err := egressRepo.UpdateEgressNodeRotationState(ctx, 999, nil, 0, ""); err == nil || err != repository.ErrNotFound {
		t.Fatalf("missing node error = %v", err)
	}
}

// 回填必须是一次性迁移:运维手动暂停(rotation_enabled=0 但 webhook 保留)后,
// 任意重启不得把它翻回开启——否则暂停开关形同虚设。
func TestRotationBackfillRunsOnceAndRespectsManualPause(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-rotation-once.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	node := egressNodeModel{Name: "once", Enabled: true, Health: 1}
	if err := database.db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	egressRepo := NewEgressRepository(database)
	// 模拟已配置 webhook 的节点被运维手动暂停。
	if err := egressRepo.UpdateEgressNodeRotationURL(ctx, node.ID, "encrypted-webhook", true); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec("UPDATE egress_nodes SET rotation_enabled = 0 WHERE id = ?", node.ID).Error; err != nil {
		t.Fatal(err)
	}
	// 重启(第二次 InitializeSchema): 标记已存在, 回填必须跳过。
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var stored egressNodeModel
	if err := database.db.First(&stored, node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RotationEnabled {
		t.Fatalf("manual rotation pause was flipped back on by restart backfill")
	}
	// 删除标记(等价于回填尚未执行过的存量库)后重启: 回填应执行并补启用。
	if err := database.db.Exec("DELETE FROM schema_migration_markers WHERE name = 'egress_rotation_enabled_backfill'").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.db.First(&stored, node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.RotationEnabled {
		t.Fatalf("first-run backfill did not enable rotation for node with webhook")
	}
}
