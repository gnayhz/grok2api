package account

import "strings"

// ImportEmail is the identity protected by an explicit account deletion.
// Protection crosses providers; unknown email is not a synthetic identity.
func ImportEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

type ImportSkipReason string

const (
	ImportTombstoned    ImportSkipReason = "tombstoned"
	ImportSourceChanged ImportSkipReason = "source_changed"
	ImportTargetChanged ImportSkipReason = "target_changed"
)

// ImportIdentityFacts are current, locked persistence facts. M07 decides
// whether explicit material may be installed; M21 gathers and commits facts.
type ImportIdentityFacts struct {
	Source           *CredentialRef
	Target           *CredentialRef
	SourceEmail      string
	TargetEmail      string
	TombstonedEmails map[string]struct{}
}

func CheckImportIdentity(seedEmail string, observedSource, observedTarget *CredentialRef, current ImportIdentityFacts) ImportSkipReason {
	if observedSource != nil && (current.Source == nil || *observedSource != *current.Source) {
		return ImportSourceChanged
	}
	if observedTarget != nil && (current.Target == nil || *observedTarget != *current.Target) {
		return ImportTargetChanged
	}
	for _, email := range []string{seedEmail, current.SourceEmail, current.TargetEmail} {
		if _, blocked := current.TombstonedEmails[ImportEmail(email)]; blocked {
			return ImportTombstoned
		}
	}
	return ""
}
