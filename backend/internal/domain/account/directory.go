package account

// Identity is the secret-free projection used to resolve administrator-facing references.
type Identity struct {
	ID       uint64
	Name     string
	Email    string
	Provider Provider
}
