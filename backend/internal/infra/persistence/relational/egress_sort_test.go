package relational

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type legacyEgressNodeWithoutProxyPool struct {
	ID                          uint64 `gorm:"primaryKey;autoIncrement"`
	Name                        string `gorm:"size:160;not null"`
	Enabled                     bool   `gorm:"not null;default:true"`
	EncryptedProxyURL           string `gorm:"type:text;not null;default:''"`
	UserAgent                   string `gorm:"size:512;not null;default:''"`
	EncryptedCloudflareCookie   string `gorm:"type:text;not null;default:''"`
	ClearanceRefreshedAt        *time.Time
	ClearanceFingerprint        string  `gorm:"size:64;not null;default:''"`
	ClearanceBindingFingerprint string  `gorm:"size:64;not null;default:''"`
	Health                      float64 `gorm:"not null;default:1"`
	FailureCount                int     `gorm:"not null;default:0"`
	CooldownUntil               *time.Time
	LastError                   string    `gorm:"size:512"`
	CreatedAt                   time.Time `gorm:"not null"`
	UpdatedAt                   time.Time `gorm:"not null"`
}

func (legacyEgressNodeWithoutProxyPool) TableName() string { return "egress_nodes" }

func TestEgressRepositorySortsInDatabase(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-sort.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)
	for _, value := range []egress.Node{
		{Name: "slow", Enabled: true, Health: 0.2},
		{Name: "healthy", Enabled: true, Health: 0.9},
		{Name: "middle", Enabled: true, Health: 0.5},
	} {
		if _, err := repo.CreateEgressNode(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	values, err := repo.ListEgressNodes(ctx, repository.SortQuery{Field: "health", Direction: repository.SortDescending})
	if err != nil || len(values) != 3 || values[0].Name != "healthy" || values[2].Name != "slow" {
		t.Fatalf("health sort = %#v, err = %v", values, err)
	}
}

func TestEgressRepositoryPaginatesAndFiltersManagementList(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)
	created := make(map[string]egress.Node)
	for _, value := range []egress.Node{
		{Name: "alpha", Enabled: true, Health: 0.9, ProbeStatus: egress.ProbeStatusHealthy},
		{Name: "beta", Enabled: false, Health: 0.4, ProbeStatus: egress.ProbeStatusUnhealthy},
		{Name: "gamma", Enabled: true, Health: 0.7, ProbeStatus: egress.ProbeStatusUnknown},
	} {
		node, createErr := repo.CreateEgressNode(ctx, value)
		if createErr != nil {
			t.Fatal(createErr)
		}
		created[node.Name] = node
	}
	if err := database.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", created["beta"].ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}

	values, total, err := repo.ListEgressNodePage(ctx, repository.EgressNodeListQuery{
		Page: repository.PageQuery{Limit: 2, Sort: repository.SortQuery{Field: "name", Direction: repository.SortAscending}},
	})
	if err != nil || total != 3 || len(values) != 2 || values[0].Name != "alpha" || values[1].Name != "beta" {
		t.Fatalf("first page = %#v, total=%d, err=%v", values, total, err)
	}
	values, total, err = repo.ListEgressNodePage(ctx, repository.EgressNodeListQuery{
		Page: repository.PageQuery{Offset: 2, Limit: 2, Sort: repository.SortQuery{Field: "name", Direction: repository.SortAscending}},
	})
	if err != nil || total != 3 || len(values) != 1 || values[0].Name != "gamma" {
		t.Fatalf("second page = %#v, total=%d, err=%v", values, total, err)
	}

	values, total, err = repo.ListEgressNodePage(ctx, repository.EgressNodeListQuery{
		Page:   repository.PageQuery{Limit: 20, Search: "ALP"},
		Filter: repository.EgressNodeListFilter{ProbeStatus: egress.ProbeStatusHealthy},
	})
	if err != nil || total != 1 || len(values) != 1 || values[0].Name != "alpha" {
		t.Fatalf("filtered page = %#v, total=%d, err=%v", values, total, err)
	}

	disabled := false
	values, total, err = repo.ListEgressNodePage(ctx, repository.EgressNodeListQuery{
		Page: repository.PageQuery{Limit: 20}, Filter: repository.EgressNodeListFilter{Enabled: &disabled},
	})
	if err != nil || total != 1 || len(values) != 1 || values[0].Name != "beta" {
		t.Fatalf("disabled page = %#v, total=%d, err=%v", values, total, err)
	}
}

func TestEgressStateUpdatesDoNotOverwriteClearanceOrHealth(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "web", Enabled: true, Health: 1, UserAgent: "old", EncryptedCloudflareCookie: "old-cookie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateEgressNodeHealth(ctx, node.ID, 0.4, 2, nil, "anti-bot rejection"); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateEgressNodeClearance(ctx, node.ID, "new-cookie", "new-agent", strings.Repeat("a", 64), strings.Repeat("b", 64), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	actual, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Health != 0.4 || actual.FailureCount != 2 || actual.EncryptedCloudflareCookie != "new-cookie" || actual.UserAgent != "new-agent" || actual.ClearanceBindingFingerprint != strings.Repeat("b", 64) {
		t.Fatalf("partial updates overwrote state: %#v", actual)
	}
}

// 新架构的出口表是无作用域资源:所有 scope/账号绑定列都不存在。
// (旧列删除迁移路径 dropLegacyEgressResourceColumns 在 SQLite 上存在非测试
// 代码缺陷——字符串表名触发 glebarez DropColumn 空指针——修复前无法在测试
// 中构造带旧列的库;这里断言目标 schema 形态。)
func TestInitializeSchemaBuildsScopeFreeEgressTables(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	migrator := database.db.WithContext(ctx).Migrator()
	if migrator.HasColumn("egress_nodes", "scope") || migrator.HasColumn("egress_nodes", "pool_id") || migrator.HasColumn("egress_nodes", "account_capacity") {
		t.Fatal("egress_nodes still carries legacy scope/binding columns")
	}
	if migrator.HasColumn("egress_pools", "scope") {
		t.Fatal("egress_pools still carries the legacy scope column")
	}
	if migrator.HasColumn("egress_subscription_sources", "scope") || migrator.HasColumn("egress_subscription_sources", "default_account_capacity") {
		t.Fatal("egress_subscription_sources still carries legacy columns")
	}
	if migrator.HasTable("egress_proxy_profiles") {
		t.Fatal("shared proxy profile table still exists")
	}
	node, err := NewEgressRepository(database).CreateEgressNode(ctx, egress.Node{Name: "scope-free", Enabled: true})
	if err != nil {
		t.Fatalf("scope-free node rejected: %v", err)
	}
	if node.Name != "scope-free" {
		t.Fatalf("created node = %#v", node)
	}
}

func TestInitializeSchemaAddsProxyPoolWithoutChangingExistingRows(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-proxy-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.db.WithContext(ctx).AutoMigrate(&legacyEgressNodeWithoutProxyPool{}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&legacyEgressNodeWithoutProxyPool{Name: "existing", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("repeated schema initialization failed: %v", err)
	}
	repo := NewEgressRepository(database)
	existing, err := repo.GetEgressNode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if existing.ProxyPool || existing.Name != "existing" || existing.ProbeProvider != "" || existing.IPv4Probe.Status != egress.ProbeStatusUnknown || existing.IPv6Probe.Status != egress.ProbeStatusUnknown {
		t.Fatalf("legacy row changed during migration: %#v", existing)
	}
	existing.ProxyPool = true
	updated, err := repo.UpdateEgressNode(ctx, existing)
	if err != nil || !updated.ProxyPool {
		t.Fatalf("proxy pool did not round trip: %#v, err=%v", updated, err)
	}
}
