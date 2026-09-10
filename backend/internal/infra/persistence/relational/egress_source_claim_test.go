package relational

import (
	"context"
	"errors"
	"math"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestSourceSyncClaimPersistsAndIsConsumedExactlyOnce(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo, other := NewEgressRepository(db), NewEgressRepository(peer)
			source, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{Name: "claim-feed", Enabled: true, EncryptedURL: "cipher", RefreshIntervalSeconds: 900})
			if err != nil {
				t.Fatal(err)
			}
			first, err := repo.BeginEgressSourceSync(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			second, err := other.BeginEgressSourceSync(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			if second.Revision <= first.Revision {
				t.Fatal("claim did not advance")
			}
			now := time.Now().UTC()
			if err := repo.FailEgressSourceSync(ctx, first, now, now.Add(time.Minute), "old failure"); !errors.Is(err, egress.ErrSourceSyncStale) {
				t.Fatalf("stale failure: %v", err)
			}
			if _, err := repo.CommitEgressSourceSync(ctx, first, nil, now, now.Add(time.Minute)); !errors.Is(err, egress.ErrSourceSyncStale) {
				t.Fatalf("stale commit: %v", err)
			}
			// A new repository instance/connection can finish the persisted claim.
			node := egress.Node{Name: "current", SourceID: source.ID, SourceKey: "current", Enabled: true, EncryptedProxyURL: "current-proxy", Health: 1}
			imported, err := repo.CommitEgressSourceSync(ctx, second, []egress.Node{node}, now, now.Add(time.Minute))
			if err != nil || imported != 1 {
				t.Fatalf("current commit: %d %v", imported, err)
			}
			before := sourceSyncSnapshot(t, peer)
			if _, err := other.CommitEgressSourceSync(ctx, second, nil, now, now); !errors.Is(err, egress.ErrSourceSyncStale) {
				t.Fatalf("duplicate success: %v", err)
			}
			if err := other.FailEgressSourceSync(ctx, second, now, now, "late failure"); !errors.Is(err, egress.ErrSourceSyncStale) {
				t.Fatalf("duplicate failure: %v", err)
			}
			if after := sourceSyncSnapshot(t, peer); !reflect.DeepEqual(before, after) {
				t.Fatal("consumed claim changed persisted state")
			}
			current, err := repo.GetEgressSource(ctx, source.ID)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := repo.BeginEgressSourceSync(ctx, current)
			if err != nil {
				t.Fatal(err)
			}
			if err := other.DeleteEgressSource(ctx, source.ID); err != nil {
				t.Fatal(err)
			}
			if err := repo.FailEgressSourceSync(ctx, claim, now, now, "late deleted failure"); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("deleted source: %v", err)
			}
			nodes, err := other.ListEgressNodes(ctx, repository.SortQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if len(nodes) != 1 || nodes[0].SourceID != 0 || nodes[0].SourceKey != "" || !nodes[0].Enabled || nodes[0].EncryptedProxyURL != "current-proxy" {
				t.Fatalf("delete lost imported node: %+v", nodes)
			}
		})
	}
}

type sourceSyncState struct {
	Sources []egressSubscriptionSourceModel
	Nodes   []egressNodeModel
	Pools   []egressPoolModel
	Members []egressPoolMemberModel
	Config  []egressOperationsConfigModel
}

func sourceSyncSnapshot(t *testing.T, db *Database) sourceSyncState {
	t.Helper()
	var state sourceSyncState
	for _, rows := range []any{&state.Sources, &state.Nodes, &state.Pools, &state.Members, &state.Config} {
		if err := db.db.Find(rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func TestSourceSyncLegacyMigrationAndRevisionExhaustion(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, peer := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewEgressRepository(db)
			source, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{Name: "legacy-source", Enabled: true, EncryptedURL: "legacy-secret", EncryptedProxyURL: "legacy-proxy", RefreshIntervalSeconds: 1800})
			if err != nil {
				t.Fatal(err)
			}
			stamp := time.Now().UTC().Add(-time.Hour)
			if err := seedLegacySourceSyncMetadata(repo, ctx, source.ID, stamp, stamp.Add(time.Minute), 7, "historical error"); err != nil {
				t.Fatal(err)
			}
			node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "legacy-node", SourceID: source.ID, SourceKey: "legacy", Enabled: true, EncryptedProxyURL: "old-node-proxy", Health: 1})
			if err != nil {
				t.Fatal(err)
			}
			before := sourceSyncSnapshot(t, peer)
			// Recreate the immediately preceding schema by removing only the new
			// constraint/column, retaining real source rows and their node foreign key.
			downgrade := func() error {
				migrator := db.db.Migrator()
				if err := migrator.DropConstraint(&egressSubscriptionSourceModel{}, "chk_egress_source_sync_revision"); err != nil {
					return err
				}
				return migrator.DropColumn(&egressSubscriptionSourceModel{}, "sync_revision")
			}
			if dialect == "sqlite" {
				err = db.withSQLiteForeignKeysDisabled(ctx, downgrade)
			} else {
				err = downgrade()
			}
			if err != nil {
				t.Fatal(err)
			}
			// Verify fixture preparation itself retained the association.
			var sourceID uint64
			if err := db.db.Model(&egressNodeModel{}).Select("source_id").Where("id = ?", node.ID).Scan(&sourceID).Error; err != nil || sourceID != source.ID {
				t.Fatalf("downgrade lost source association: %d %v", sourceID, err)
			}
			for i := 0; i < 2; i++ {
				if err := db.InitializeSchema(ctx); err != nil {
					t.Fatal(err)
				}
			}
			// Application startup opens fresh connections after schema migration.
			db = reopenSourceDatabase(t, db)
			peer = reopenSourceDatabase(t, peer)
			repo = NewEgressRepository(db)
			after := sourceSyncSnapshot(t, peer)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("migration changed existing state: before=%+v after=%+v", before, after)
			}
			current, err := repo.GetEgressSource(ctx, source.ID)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := NewEgressRepository(peer).BeginEgressSourceSync(ctx, current)
			if err != nil || claim.Revision != 1 {
				t.Fatalf("migrated claim: %+v %v", claim, err)
			}
			if err := repo.FailEgressSourceSync(ctx, claim, stamp, stamp.Add(time.Minute), "current failure"); err != nil {
				t.Fatal(err)
			}
			if err := db.db.Model(&egressSubscriptionSourceModel{}).Where("id = ?", source.ID).UpdateColumn("sync_revision", uint64(math.MaxInt64-1)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := repo.BeginEgressSourceSync(ctx, current); err == nil {
				t.Fatal("exhausted revision wrapped")
			}
			current.Name += "-last"
			current, err = repo.UpdateEgressSource(ctx, current)
			if err != nil || current.SyncRevision != math.MaxInt64 {
				t.Fatalf("last configuration revision: %v %v", current.SyncRevision, err)
			}
			current.Name += "-overflow"
			if _, err := repo.UpdateEgressSource(ctx, current); err == nil {
				t.Fatal("configuration revision wrapped")
			}
			if err := repo.DeleteEgressSource(ctx, source.ID); err != nil {
				t.Fatal(err)
			}
			stored, err := repo.GetEgressNode(ctx, node.ID)
			if err != nil || stored.SourceID != 0 {
				t.Fatalf("migrated source FK: %+v %v", stored, err)
			}
		})
	}
}

func reopenSourceDatabase(t *testing.T, old *Database) *Database {
	t.Helper()
	ctx := context.Background()
	var db *Database
	var err error
	if old.dialect == "postgres" {
		var schema string
		if err := old.db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(os.Getenv("TEST_POSTGRES_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = OpenPostgres(ctx, parsed.String(), 4, 4)
	} else {
		var rows []struct {
			Seq  int
			Name string
			File string
		}
		if err := old.db.Raw("PRAGMA database_list").Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		path := ""
		for _, row := range rows {
			if row.Name == "main" {
				path = row.File
			}
		}
		if path == "" {
			t.Fatal("missing source database path")
		}
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = OpenSQLite(ctx, path)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
