package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The caller holds the shared maintenance barrier. All account rows precede
// credential rows in the lock order used by import, refresh and deletion.
func lockAccountImportFacts(tx *gorm.DB, inputs []repository.AccountImport, byIdentity, bySource map[string]accountModel) (map[uint64]accountModel, map[uint64]uint64, map[string]struct{}, error) {
	ids := make([]uint64, 0, len(byIdentity)+len(bySource)+len(inputs))
	emails := make([]string, 0, len(inputs)+len(ids))
	for _, row := range byIdentity {
		ids = append(ids, row.ID)
	}
	for _, row := range bySource {
		ids = append(ids, row.ID)
	}
	for _, input := range inputs {
		emails = append(emails, input.Credential.Email)
		if input.Source != nil {
			ids = append(ids, input.Source.AccountID)
		}
		if input.Target != nil {
			ids = append(ids, input.Target.AccountID)
		}
	}
	current := make(map[uint64]accountModel, len(ids))
	generations := make(map[uint64]uint64, len(ids))
	if len(ids) > 0 {
		var rows []accountModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", uniqueSortedIDs(ids)).Order("id ASC").Find(&rows).Error; err != nil {
			return nil, nil, nil, err
		}
		for _, row := range rows {
			current[row.ID] = row
			emails = append(emails, row.Email)
		}
		var material []struct {
			AccountID  uint64
			Generation uint64
		}
		if err := tx.Model(&accountCredentialModel{}).Select("account_id", "generation").Where("account_id IN ?", uniqueSortedIDs(ids)).Find(&material).Error; err != nil {
			return nil, nil, nil, err
		}
		for _, row := range material {
			generations[row.AccountID] = row.Generation
		}
	}
	tombstoned, err := tombstonedEmails(tx, emails)
	return current, generations, tombstoned, err
}

func accountImportSkip(input repository.AccountImport, target *accountModel, current map[uint64]accountModel, generations map[uint64]uint64, tombstoned map[string]struct{}) account.ImportSkipReason {
	facts := account.ImportIdentityFacts{TombstonedEmails: tombstoned}
	if source := input.Source; source != nil {
		row, found := current[source.AccountID]
		generation, materialFound := generations[source.AccountID]
		if found && materialFound {
			facts.Source = &account.CredentialRef{AccountID: row.ID, Provider: account.Provider(row.Provider), Generation: generation}
			facts.SourceEmail = row.Email
		}
	}
	if target != nil {
		facts.TargetEmail = target.Email
		if generation, found := generations[target.ID]; found {
			facts.Target = &account.CredentialRef{AccountID: target.ID, Provider: account.Provider(target.Provider), Generation: generation}
		}
	}
	return account.CheckImportIdentity(input.Credential.Email, input.Source, input.Target, facts)
}

func normalizedImportEmails(emails []string) []string {
	normalized := make([]string, 0, len(emails))
	seen := make(map[string]struct{}, len(emails))
	for _, raw := range emails {
		email := account.ImportEmail(raw)
		if email == "" {
			continue
		}
		if _, found := seen[email]; !found {
			seen[email] = struct{}{}
			normalized = append(normalized, email)
		}
	}
	return normalized
}

// TombstonedEmails is a preflight/read projection, never an import permit.
func (r *AccountRepository) TombstonedEmails(ctx context.Context, emails []string) (map[string]struct{}, error) {
	return tombstonedEmails(r.db.db.WithContext(ctx), emails)
}

func tombstonedEmails(tx *gorm.DB, emails []string) (map[string]struct{}, error) {
	normalized := normalizedImportEmails(emails)
	out := make(map[string]struct{})
	if len(normalized) == 0 {
		return out, nil
	}
	var rows []string
	if err := tx.Model(&accountTombstoneModel{}).Where("LOWER(email) IN ?", normalized).Pluck("LOWER(email)", &rows).Error; err != nil {
		return nil, err
	}
	for _, email := range rows {
		out[email] = struct{}{}
	}
	return out, nil
}

// Only explicit deletion calls this, after locking and media validation of the
// final set. Intent and account removal commit together; auto-clean omits it.
func writeDeletedAccountTombstones(tx *gorm.DB, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	var rows []accountModel
	if err := tx.Select("id", "email", "provider", "name").Where("id IN ?", ids).Order("id ASC").Find(&rows).Error; err != nil {
		return err
	}
	models := make([]accountTombstoneModel, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	now := time.Now().UTC()
	for _, row := range rows {
		email := account.ImportEmail(row.Email)
		if email == "" {
			continue
		}
		if _, found := seen[email]; found {
			continue
		}
		seen[email] = struct{}{}
		models = append(models, accountTombstoneModel{Email: email, Provider: row.Provider, Name: row.Name, DeletedAt: now})
	}
	if len(models) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "email"}},
		DoUpdates: clause.AssignmentColumns([]string{"provider", "name", "deleted_at"}),
	}).Create(&models).Error
}
