package account

// IdentityLink is a persisted association between provider faces of one
// account identity. It is an undirected fact, never inferred from names or
// email addresses. Account storage validates providers and owns link writes.
type IdentityLink struct {
	AccountID        uint64
	RelatedAccountID uint64
}
