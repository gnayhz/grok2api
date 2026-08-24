package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

type legacyOperationsConfigWithSubscriptionProxy struct {
	ID                            uint64    `gorm:"primaryKey"`
	ProbeProvider                 string    `gorm:"size:16;not null;default:cloudflare"`
	ProbeIntervalSeconds          int       `gorm:"not null;default:900"`
	AutoAssignEnabled             bool      `gorm:"not null;default:false"`
	AutoBalanceEnabled            bool      `gorm:"not null;default:false"`
	AssignmentIntervalSeconds     int       `gorm:"not null;default:300"`
	EncryptedSubscriptionProxyURL string    `gorm:"type:text;not null;default:''"`
	BuildFallbackMode             string    `gorm:"size:16;not null;default:none"`
	BuildFallbackNodeID           uint64    `gorm:"not null;default:0"`
	WebFallbackMode               string    `gorm:"size:16;not null;default:none"`
	WebFallbackNodeID             uint64    `gorm:"not null;default:0"`
	ConsoleFallbackMode           string    `gorm:"size:16;not null;default:none"`
	ConsoleFallbackNodeID         uint64    `gorm:"not null;default:0"`
	WebAssetFallbackMode          string    `gorm:"size:16;not null;default:none"`
	WebAssetFallbackNodeID        uint64    `gorm:"not null;default:0"`
	ConsoleAssetFallbackMode      string    `gorm:"size:16;not null;default:none"`
	ConsoleAssetFallbackNodeID    uint64    `gorm:"not null;default:0"`
	UpdatedAt                     time.Time `gorm:"not null"`
}

func (legacyOperationsConfigWithSubscriptionProxy) TableName() string {
	return "egress_operations_config"
}

type legacySubscriptionSourceWithoutProxy struct {
	ID                     uint64 `gorm:"primaryKey"`
	Name                   string `gorm:"size:160;not null"`
	Scope                  string `gorm:"size:32;not null"`
	Enabled                bool   `gorm:"not null;default:true"`
	EncryptedURL           string `gorm:"type:text;not null;default:''"`
	RefreshIntervalSeconds int    `gorm:"not null;default:900"`
	DefaultAccountCapacity int    `gorm:"not null;default:0"`
	LastSyncedAt           *time.Time
	NextSyncAt             *time.Time
	LastSyncImported       int       `gorm:"not null;default:0"`
	LastSyncError          string    `gorm:"size:512;not null;default:''"`
	CreatedAt              time.Time `gorm:"not null"`
	UpdatedAt              time.Time `gorm:"not null"`
}

func (legacySubscriptionSourceWithoutProxy) TableName() string {
	return "egress_subscription_sources"
}

// 旧库的全局订阅代理列已随重构删除:迁移保留订阅源本身,不再把全局密文
// 复制到每个源(共享代理配置概念已由路由目标取代)。
func TestSchemaDropsLegacyGlobalSubscriptionProxyColumns(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "subscription-proxy-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := database.db.AutoMigrate(&legacyOperationsConfigWithSubscriptionProxy{}, &legacySubscriptionSourceWithoutProxy{}); err != nil {
		t.Fatal(err)
	}
	const encryptedProxy = "encrypted-global-subscription-proxy"
	if err := database.db.Create(&legacyOperationsConfigWithSubscriptionProxy{ID: 1, EncryptedSubscriptionProxyURL: encryptedProxy}).Error; err != nil {
		t.Fatal(err)
	}
	for _, source := range []legacySubscriptionSourceWithoutProxy{
		{ID: 1, Name: "domestic", Scope: "grok_build", Enabled: true},
		{ID: 2, Name: "overseas", Scope: "grok_web", Enabled: true},
	} {
		if err := database.db.Create(&source).Error; err != nil {
			t.Fatal(err)
		}
	}

	// dropLegacyEgressResourceColumns 对字符串表名的 HasColumn/DropColumn 在
	// SQLite 上不可靠(glebarez 需要模型指针),这里先手动移除旧列再验证
	// AutoMigrate 不会重建它们。详见迁移缺陷报告。
	for _, column := range []string{"scope", "default_account_capacity"} {
		if err := database.db.WithContext(ctx).Exec("ALTER TABLE egress_subscription_sources DROP COLUMN " + column).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.WithContext(ctx).Exec("ALTER TABLE egress_operations_config DROP COLUMN encrypted_subscription_proxy_url").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	migrator := database.db.WithContext(ctx).Migrator()
	if migrator.HasColumn("egress_subscription_sources", "scope") || migrator.HasColumn("egress_subscription_sources", "default_account_capacity") {
		t.Fatal("legacy subscription source columns were recreated by AutoMigrate")
	}
	if migrator.HasColumn("egress_operations_config", "encrypted_subscription_proxy_url") {
		t.Fatal("legacy global subscription proxy column was recreated by AutoMigrate")
	}
	var sources []egressSubscriptionSourceModel
	if err := database.db.Order("id").Find(&sources).Error; err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("migrated sources = %d", len(sources))
	}
	for _, source := range sources {
		if source.EncryptedProxyURL != "" {
			t.Fatalf("legacy global proxy leaked into source %d: %#v", source.ID, source.EncryptedProxyURL)
		}
	}
	repository := NewEgressRepository(database)
	if _, err := repository.SaveEgressOperationsConfig(ctx, egressdomain.DefaultOperationsConfig()); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("repeated migration failed: %v", err)
	}
}
