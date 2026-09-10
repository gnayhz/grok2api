package repository

import (
	"context"
	"time"
)

// JournalScope includes tenant, session and upstream plane in its opaque Key.
// The normalizer version is part of persisted identity, not a cache hint.
type JournalScope struct {
	Model, Key string
	Normalizer int
}

type JournalReserve struct {
	Scope JournalScope
	// LegacyScopes are authenticated prior identities of this same conversation.
	LegacyScopes            []JournalScope
	Token, ParentResponseID string
	// Prefixes contains one cumulative digest per visible input item. Reasoning
	// is excluded from matching, but retained verbatim in encrypted deltas.
	Prefixes []string
	BaseHash string
	// ItemHash enables one-time migration of legacy configuration-bound checkpoints.
	ItemHash func([]byte) (string, bool, error)
	// Incremental is native previous_response_id input. ItemHashes are chained
	// after the explicitly selected parent's digest inside the transaction.
	Incremental      bool
	ItemHashes       []string
	Input            [][]byte
	InputReasoning   map[int][][]byte
	Now              time.Time
	Retention, Lease time.Duration
}

type JournalTicket struct {
	Scope                    JournalScope
	Token, Parent, InputHash string
	Generation, Version      int64
	InputCount               int
}

type JournalTurn struct {
	ID, ResponseID, Parent, PrefixHash string
	InputCount, TotalCount             int
	Input, Output                      [][]byte
}

type JournalReservation struct {
	Ticket  JournalTicket
	Turns   []JournalTurn
	Outcome string
}

type JournalCommit struct {
	Ticket                 JournalTicket
	ResponseID, PrefixHash string
	TotalCount             int
	Output                 [][]byte
	Now                    time.Time
}

// JournalSnapshot contains one current, unexpired generation's immutable turns.
// It carries no permission to copy them into another identity.
type JournalSnapshot struct{ Turns []JournalTurn }

type ConversationJournal interface {
	Inspect(context.Context, []JournalScope, time.Time) ([]JournalSnapshot, error)
	Reserve(context.Context, JournalReserve) (JournalReservation, error)
	Commit(context.Context, JournalCommit) error
	Reset(context.Context, JournalScope, time.Time, ...int64) error
	Release(context.Context, JournalTicket) error
	Prune(context.Context, time.Time, int) (int64, error)
}
