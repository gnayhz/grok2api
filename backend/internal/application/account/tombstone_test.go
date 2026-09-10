package account

import (
	"context"
	"path/filepath"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// tombstoneImportAdapter 单账号导入适配器(email 可控)。
type tombstoneImportAdapter struct {
	email string
}

func (tombstoneImportAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }

func (tombstoneImportAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:       accountdomain.ProviderBuild,
		ModelNamespace: accountdomain.ProviderBuild.ModelNamespace(),
		Credential: provider.CredentialSurface{
			AuthType: accountdomain.AuthTypeOAuth,
			Import:   true,
			Refresh:  true,
		},
	}
}

func (a tombstoneImportAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return []provider.CredentialSeed{{
		Name: "resurrect-test", Email: a.email, SourceKey: "tomb:test", OIDCClientID: "client", RefreshToken: "rt",
	}}, nil
}

func (tombstoneImportAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}

// TestDeletedAccountNotResurrectedByImport 锚定批9 事故修复:
// 手动删除写墓碑 → 同 email 导入被跳过 → 清墓碑后可再导入。
func TestDeletedAccountNotResurrectedByImport(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "tombstone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(tombstoneImportAdapter{email: "ghost@example.com"}), cipher, nil)

	// ① 首次导入:创建成功。
	first, err := service.ImportCredentials(ctx, []byte("doc"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 {
		t.Fatalf("首次导入应创建 1 个, got %#v", first)
	}

	// ② 手动删除:写墓碑。
	ids := first.AccountIDs
	deleted, err := service.BatchDelete(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("应删除 1 个, got %d", deleted)
	}
	tombstoned, err := repo.TombstonedEmails(ctx, []string{"ghost@example.com"})
	if err != nil || len(tombstoned) != 1 {
		t.Fatalf("墓碑应命中, got %v err=%v", tombstoned, err)
	}

	// ③ 再次导入同 email:被墓碑跳过,不复活。
	second, err := service.ImportCredentials(ctx, []byte("doc"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 || second.Skipped != 1 {
		t.Fatalf("墓碑应拦截复活, got %#v", second)
	}

	// ④ 清墓碑:恢复导入通道。
	cleared, err := repo.ClearTombstones(ctx, []string{"ghost@example.com"})
	if err != nil || cleared != 1 {
		t.Fatalf("清墓碑 = %d err=%v", cleared, err)
	}
	third, err := service.ImportCredentials(ctx, []byte("doc"))
	if err != nil {
		t.Fatal(err)
	}
	if third.Created != 1 {
		t.Fatalf("清墓碑后应可再导入, got %#v", third)
	}
}
