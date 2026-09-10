package relational

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAccountIdentityProjectionAcrossInstances(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b, db := qualityManagementPair(t, dialect)
			ctx := context.Background()
			repo := NewAccountRepository(db)
			digest := strings.Repeat("a", 64)
			web := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web", SourceKey: "sso:" + digest})
			build := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: account.ProviderBuild, Name: "build", SourceKey: "build"})
			console := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console", SourceKey: "console-sso:" + digest})
			if err := repo.LinkWebToBuild(ctx, web.CredentialRef(), build.CredentialRef()); err != nil {
				t.Fatal(err)
			}
			if err := repo.ReconcileProviderLinks(ctx, console.ID); err != nil {
				t.Fatal(err)
			}
			if err := a.RefreshIdentityGroups(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.RefreshState(ctx); err != nil {
				t.Fatal(err)
			}
			want := []uint64{web.ID, build.ID, console.ID}
			slices.Sort(want)
			group, members := b.IdentityGroupOf(build.ID)
			if !reflect.DeepEqual(members, want) {
				t.Fatalf("links not projected: %v", members)
			}
			alternate := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: account.ProviderBuild, Name: "alternate", SourceKey: "alternate"})
			if err := repo.LinkWebToBuild(ctx, web.CredentialRef(), alternate.CredentialRef()); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("multiple Builds in identity accepted: %v", err)
			}
			if err := repo.LinkWebToBuild(ctx, console.CredentialRef(), alternate.CredentialRef()); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("invalid face accepted: %v", err)
			}
			// Reading failure must not erase the last committed projection.
			if err := db.db.Migrator().RenameTable("web_console_account_links", "e10_hidden_links"); err != nil {
				t.Fatal(err)
			}
			refreshErr := a.RefreshIdentityGroups(ctx)
			restoreErr := db.db.Migrator().RenameTable("e10_hidden_links", "web_console_account_links")
			if restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if refreshErr == nil {
				t.Fatal("missing fact table silently accepted")
			}
			if id, got := a.IdentityGroupOf(build.ID); id != group || !reflect.DeepEqual(got, want) {
				t.Fatalf("failed refresh changed projection: %d %v", id, got)
			}
			if err := repo.Delete(ctx, web.ID); err != nil {
				t.Fatal(err)
			}
			if err := a.RefreshIdentityGroups(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.RefreshState(ctx); err != nil {
				t.Fatal(err)
			}
			for _, id := range []uint64{build.ID, console.ID} {
				if g, got := b.IdentityGroupOf(id); g != id || !reflect.DeepEqual(got, []uint64{id}) {
					t.Fatalf("cascade deletion left a group: %d %v", g, got)
				}
			}
			links, err := repo.ListIdentityLinks(ctx)
			if err != nil || len(links) != 0 {
				t.Fatalf("dangling account links: %v %v", links, err)
			}
		})
	}
}

func TestAccountIdentityFactsUseOneCommittedSnapshot(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewAccountRepository(db)
			var webs, builds, consoles []uint64
			refs := make(map[uint64]account.CredentialRef)
			for i := range 2 {
				for _, face := range []account.Provider{account.ProviderWeb, account.ProviderBuild, account.ProviderConsole} {
					key := fmt.Sprintf("%s-%d", face, i)
					c := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{Provider: face, Name: key, SourceKey: key})
					refs[c.ID] = c.CredentialRef()
					switch face {
					case account.ProviderWeb:
						webs = append(webs, c.ID)
					case account.ProviderBuild:
						builds = append(builds, c.ID)
					case account.ProviderConsole:
						consoles = append(consoles, c.ID)
					}
				}
			}
			replace := func(i int) error {
				return db.db.Transaction(func(tx *gorm.DB) error {
					if err := lockAccountLinkMutation(tx); err != nil {
						return err
					}
					if err := tx.Exec("DELETE FROM account_provider_links").Error; err != nil {
						return err
					}
					if err := tx.Exec("DELETE FROM web_console_account_links").Error; err != nil {
						return err
					}
					// Use the same validated account operations inside the transaction.
					txRepo := NewAccountRepository(&Database{db: tx, dialect: dialect})
					if err := txRepo.LinkWebToBuild(ctx, refs[webs[i]], refs[builds[i]]); err != nil {
						return err
					}
					return linkWebToConsole(tx, webs[i], consoles[i])
				})
			}
			if err := replace(0); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				for i := range 60 {
					if err := replace(i % 2); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			reader := NewAccountRepository(peer)
			var readErr error
			for range 100 {
				links, err := reader.ListIdentityLinks(ctx)
				if err != nil {
					readErr = err
					break
				}
				if len(links) != 2 || links[0].AccountID != links[1].AccountID {
					readErr = fmt.Errorf("mixed pre/post mutation facts: %v", links)
					break
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
		})
	}
}
