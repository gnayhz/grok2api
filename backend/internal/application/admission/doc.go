// Package admission owns wait-before-delivery, guard snapshots and the
// account-switch retry budget. Gateway maps ErrDeadline onto HTTP and runs the
// physical attempt loop.
package admission
